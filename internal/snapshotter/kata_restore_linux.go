//go:build linux

package snapshotter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	katach "github.com/AgentNaut/SandboxFleet/internal/runtime/kata/ch"
	"github.com/AgentNaut/SandboxFleet/internal/runtime/kata/overlay"
	"golang.org/x/sys/unix"
)

// kataVMRestoreTimeout covers Copy (eager) restore of guest RAM on slow CI disks.
// Worker HTTP request contexts often have no deadline, so this is the real bound.
const kataVMRestoreTimeout = 5 * time.Minute

// saveSelfManagedSnapshot checkpoints a micro-VM this Worker owns: pause the
// guest, write the CH snapshot, record the frozen base id, then tear the VMM
// down (substrate CheckpointWorkload). The guest deliberately does NOT resume —
// nothing reattaches to a snapshot's paused VMM, and holding guest RAM after the
// checkpoint is what OOMs small Workers.
func (k *Kata) saveSelfManagedSnapshot(ctx context.Context, req SaveRequest) error {
	name, ok := StripPrefix(req.ID.Value, kataIDPrefix)
	if !ok {
		return fmt.Errorf("not a kata runtime id: %q", req.ID.Value)
	}
	inst, err := k.loadInstance(name)
	if err != nil {
		return fmt.Errorf("read kata instance %q: %w", name, err)
	}
	containerID := firstNonEmpty(inst.ContainerID, req.ContainerID)
	if containerID == "" {
		return fmt.Errorf("kata instance %q has empty containerID", name)
	}
	baseID := firstNonEmpty(inst.BaseID, name)
	if err := os.MkdirAll(req.DestDir, 0o755); err != nil {
		return err
	}

	// Flush guest dirty pages before Pause so overlay-upper writes (tmpfs) are
	// resident in the memory image CH is about to capture. Without this, a
	// freshly written file can restore as empty under memory pressure.
	if err := syncGuestBeforeCheckpoint(ctx, inst.VsockPath, containerID); err != nil {
		log.Printf("kata checkpoint %s: guest sync before pause: %v (continuing)", name, err)
	}

	client := newCHClient(inst.APISocket)
	if err := client.Pause(ctx); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	if err := client.Snapshot(ctx, req.DestDir); err != nil {
		return fmt.Errorf("snapshot vm: %w", err)
	}
	// Nested checkpoint of a restored VM: CH may emit a sparse memory-ranges
	// (only pages dirtied since this VMM started). Overlay that delta onto the
	// restore-source image so the snapshot is self-contained — same as substrate
	// CheckpointWorkload's MergeDeltaIntoBase. Cold-boot parents have no
	// SnapshotDir and skip this.
	if inst.SnapshotDir != "" {
		base := filepath.Join(inst.SnapshotDir, katach.MemoryRangesFile)
		delta := filepath.Join(req.DestDir, katach.MemoryRangesFile)
		if _, err := os.Stat(base); err == nil {
			tMerge := time.Now()
			if err := katach.MergeDeltaIntoBase(ctx, base, delta); err != nil {
				return fmt.Errorf("merge nested memory-ranges: %w", err)
			}
			log.Printf("kata checkpoint %s: merged restore-source memory-ranges (%s)", name, time.Since(tMerge).Round(time.Millisecond))
		}
	}
	if err := os.WriteFile(filepath.Join(req.DestDir, kataBaseIDFile), []byte(baseID), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", kataBaseIDFile, err)
	}

	// Nothing rootfs-related ships: the overlay upper is a guest tmpfs (already
	// in the memory image) and the RO lower is rebuilt from the image at restore.
	// The share is recorded only as the find-paths hint restore lays it back at.
	//
	// AppContainerName must stay the stable virtio-fs carrier id (find-paths
	// freezes <carrier>/rootfs). Prefer TrimSuffix(containerID) over
	// req.AppContainerName: the Worker passes Identity.Name, which on nested
	// forks is the child CR name and would break grandchild restore.
	meta := kataMeta{
		SourceSandboxID:  req.ID.Value,
		ContainerID:      containerID,
		AppImage:         firstNonEmpty(req.AppImage, inst.AppImage),
		AppContainerName: kataStableCarrierName(containerID, req.AppContainerName),
		BaseID:           baseID,
		VirtiofsShares:   []virtiofsShare{{Tag: overlay.FsTag, SharedDir: overlay.SharedDir(baseID)}},
		NetDevices:       readNetDevicesFromConfig(filepath.Join(req.DestDir, "config.json")),
		SavedAt:          time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(req.DestDir, kataMetaFile), raw, 0o600); err != nil {
		return err
	}

	// The snapshot is on disk, so teardown is best-effort. The instance record
	// stays until Delete so slot release can still find (and re-sweep) it.
	k.teardownInstance(ctx, inst)
	log.Printf("kata checkpoint %s: snapshot written, VMM torn down (base=%s)", name, baseID)
	return nil
}

// syncGuestBeforeCheckpoint runs sync(1) in the overlay workload so recent
// rootfs writes are settled in guest RAM before CH Pause+Snapshot.
func syncGuestBeforeCheckpoint(ctx context.Context, vsockPath, containerID string) error {
	if vsockPath == "" || containerID == "" {
		return fmt.Errorf("vsockPath and containerID are required")
	}
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := execViaAgent(syncCtx, vsockPath, containerID, []string{"sync"})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sync exit %d stderr=%s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (k *Kata) LoadSnapshot(ctx context.Context, req LoadRequest) (sandboxruntime.ID, error) {
	if req.SourceDir == "" {
		return sandboxruntime.ID{}, fmt.Errorf("source dir is required")
	}
	meta, err := readKataMeta(req.SourceDir)
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("read snapshot meta: %w", err)
	}
	if meta.ContainerID == "" {
		return sandboxruntime.ID{}, fmt.Errorf("snapshot meta missing containerID")
	}

	name := RestoredName(req.Identity)
	vmDir := filepath.Join(k.StateDir, name)
	bundle := k.bundleDir(name)
	teardownImageRootfs(bundle)
	_ = os.RemoveAll(vmDir)
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		return sandboxruntime.ID{}, err
	}

	snapDir := filepath.Join(vmDir, "snapshot")
	// Prefer rename over copyDir: SourceDir is a disposable download temp, and
	// duplicating memory-ranges into page cache routinely OOMs small CI Workers.
	if err := placeSnapshotDir(req.SourceDir, snapDir); err != nil {
		_ = os.RemoveAll(vmDir)
		return sandboxruntime.ID{}, fmt.Errorf("place snapshot: %w", err)
	}

	baseID := readKataBaseID(snapDir, meta)
	if baseID == "" {
		_ = os.RemoveAll(vmDir)
		return sandboxruntime.ID{}, fmt.Errorf("snapshot has no %s (base sandbox id) — retake it with a current Worker", kataBaseIDFile)
	}
	appImage := firstNonEmpty(meta.AppImage, req.AppImage)
	if appImage == "" {
		_ = os.RemoveAll(vmDir)
		return sandboxruntime.ID{}, fmt.Errorf("snapshot meta missing appImage (needed to rebuild the rootfs lower)")
	}
	carrierID := kataCarrierID(meta)
	if carrierID == "" {
		_ = os.RemoveAll(vmDir)
		return sandboxruntime.ID{}, fmt.Errorf("snapshot meta missing appContainerName")
	}

	cleanup := func() {
		teardownImageRootfs(bundle)
		_ = os.RemoveAll(bundle)
		_ = os.RemoveAll(vmDir)
	}
	// Rebuild the RO lower from the image at the frozen find-paths location
	// SharedDir(baseID)/<carrier>/rootfs: the guest's virtio-fs handles are
	// pinned to those paths, and a deterministic unpack reproduces them here.
	bundleRootfs := filepath.Join(bundle, "rootfs")
	if err := setupImageRootfs(ctx, appImage, bundle); err != nil {
		cleanup()
		return sandboxruntime.ID{}, fmt.Errorf("compose rootfs for %q: %w", appImage, err)
	}
	if err := os.WriteFile(filepath.Join(bundleRootfs, "etc", "resolv.conf"), []byte(gvisorRestoreResolvConf), 0o644); err != nil {
		cleanup()
		return sandboxruntime.ID{}, fmt.Errorf("write guest resolv.conf: %w", err)
	}
	if err := overlay.ReconstructSharedDirFromImage(ctx, bundleRootfs, baseID, carrierID); err != nil {
		cleanup()
		return sandboxruntime.ID{}, fmt.Errorf("stage overlay lower: %w", err)
	}

	plannedFS, err := rewriteRestoreSockets(snapDir, vmDir, meta.VirtiofsShares)
	if err != nil {
		cleanup()
		return sandboxruntime.ID{}, err
	}
	for i := range plannedFS {
		if isKataSharedTag(plannedFS[i].Tag) || plannedFS[i].SharedDir == "" {
			plannedFS[i].SharedDir = overlay.SharedDir(baseID)
		}
	}

	vfsdCmds, err := k.startVirtiofsDaemons(ctx, vmDir, plannedFS)
	if err != nil {
		cleanup()
		return sandboxruntime.ID{}, err
	}
	failed := true
	defer func() {
		if failed {
			killCmds(vfsdCmds)
			cleanup()
		}
	}()

	nets, tapFiles, err := createRestoreTaps(req.SlotID, meta.NetDevices)
	if err != nil {
		return sandboxruntime.ID{}, err
	}
	defer closeFiles(tapFiles)

	apiSocket := filepath.Join(vmDir, "clh-api.sock")
	vsockPath := filepath.Join(vmDir, "clh.sock")
	chLogPath := filepath.Join(vmDir, "cloud-hypervisor.log")
	chErrPath := filepath.Join(vmDir, "cloud-hypervisor.stderr.log")
	chErr, err := os.OpenFile(chErrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("open cloud-hypervisor stderr log: %w", err)
	}
	// CH keeps its own dup once started; holding ours would leak an fd per restore.
	defer func() { _ = chErr.Close() }()
	// -vv + --log-file: default CH is nearly silent on stdout/stderr; CI hangs
	// otherwise leave an empty cloud-hypervisor.log with no diagnostics.
	cmd := exec.Command(k.CloudHypervisorPath,
		"-vv",
		"--log-file", chLogPath,
		"--api-socket", apiSocket,
	)
	cmd.Stderr = chErr
	startedAt := time.Now()
	log.Printf("kata restore %s: starting cloud-hypervisor api=%s snap=%s", name, apiSocket, snapDir)
	if fi, statErr := os.Stat(filepath.Join(snapDir, "memory-ranges")); statErr == nil {
		log.Printf("kata restore %s: memory-ranges size=%d", name, fi.Size())
	}
	// Deliberately not CommandContext: the VMM must outlive the restore RPC.
	if err := cmd.Start(); err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	defer func() {
		if failed && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	client := newCHClient(apiSocket)
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.WaitReady(waitCtx, 30*time.Second); err != nil {
		_ = chErr.Sync()
		return sandboxruntime.ID{}, withCHLog(err, chErr, chLogPath, chErrPath)
	}
	log.Printf("kata restore %s: cloud-hypervisor ready after %s", name, time.Since(startedAt).Round(time.Millisecond))
	// Copy (eager) loads guest RAM before resume so post-restore Exec (e.g. python /readyz)
	// is not stalled by nested-virt OnDemand page faults. CH enum is Copy|OnDemand only.
	// Dedicated deadline: parent ctx from Worker HTTP usually has none.
	restoreCtx, restoreCancel := context.WithTimeout(ctx, kataVMRestoreTimeout)
	defer restoreCancel()
	restoreAt := time.Now()
	log.Printf("kata restore %s: vm.restore begin mode=Copy timeout=%s", name, kataVMRestoreTimeout)
	if err := client.restoreVMWithNetworkFDs(restoreCtx, snapDir, nets, "Copy"); err != nil {
		_ = chErr.Sync()
		log.Printf("kata restore %s: vm.restore failed after %s: %v", name, time.Since(restoreAt).Round(time.Millisecond), err)
		return sandboxruntime.ID{}, withCHLog(fmt.Errorf("vm.restore: %w", err), chErr, chLogPath, chErrPath)
	}
	log.Printf("kata restore %s: vm.restore ok after %s", name, time.Since(restoreAt).Round(time.Millisecond))
	if err := client.Resume(ctx); err != nil {
		_ = chErr.Sync()
		return sandboxruntime.ID{}, withCHLog(fmt.Errorf("resume restored vm: %w", err), chErr, chLogPath, chErrPath)
	}
	log.Printf("kata restore %s: resume ok total=%s", name, time.Since(startedAt).Round(time.Millisecond))

	inst := kataInstance{
		Name:        name,
		Namespace:   req.Identity.Namespace,
		SandboxName: req.Identity.Name,
		UID:         string(req.Identity.UID),
		SlotID:      req.SlotID,
		VMDir:       vmDir,
		APISocket:   apiSocket,
		VsockPath:   vsockPath,
		SnapshotDir: snapDir,
		ContainerID: meta.ContainerID,
		BaseID:      baseID,
		AppImage:    appImage,
		BundleDir:   bundle,
		PID:         cmd.Process.Pid,
	}
	if err := k.saveInstance(inst); err != nil {
		return sandboxruntime.ID{}, err
	}
	failed = false

	restoredID := sandboxruntime.ID{Value: kataIDPrefix + name}
	ac, err := dialAgentRetry(ctx, vsockPath, 45*time.Second)
	if err != nil {
		_ = k.DeleteRestored(ctx, restoredID)
		return sandboxruntime.ID{}, fmt.Errorf("wait kata-agent: %w", err)
	}
	// Best-effort guest networking so restored children can egress. The address
	// is unique per SlotID so concurrent restores do not collide.
	_ = configureGuestNetwork(ctx, ac, req.SlotID)
	_ = ac.Close()
	return restoredID, nil
}

func (k *Kata) DeleteRestored(ctx context.Context, id sandboxruntime.ID) error {
	name, ok := StripPrefix(id.Value, kataIDPrefix)
	if !ok {
		return fmt.Errorf("not a kata runtime id: %q", id.Value)
	}
	inst, err := k.loadInstance(name)
	if err != nil {
		return nil
	}
	k.teardownInstance(ctx, inst)
	_ = os.Remove(k.instancePath(name))
	return nil
}

// teardownInstance releases everything one micro-VM holds: the VM + VMM behind
// the api-socket, the CH process, the per-sandbox host state (which also kills
// the orphaned virtiofsd, matched by the sandbox id in its cmdline), and the
// image bundle overlay. Best-effort and idempotent — checkpoint and delete both
// run it.
func (k *Kata) teardownInstance(ctx context.Context, inst kataInstance) {
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if inst.APISocket != "" {
		_ = newCHClient(inst.APISocket).Shutdown(shutCtx)
	}
	if inst.PID > 0 {
		_ = unix.Kill(inst.PID, unix.SIGKILL)
	}
	if inst.BaseID != "" {
		overlay.CleanupSandboxState(shutCtx, inst.BaseID)
	}
	teardownImageRootfs(inst.BundleDir)
	if inst.BundleDir != "" {
		_ = os.RemoveAll(inst.BundleDir)
	}
	if inst.VMDir != "" && strings.HasPrefix(inst.VMDir, k.StateDir) {
		_ = os.RemoveAll(inst.VMDir)
	}
}

func (k *Kata) ExecRestored(ctx context.Context, id sandboxruntime.ID, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	name, ok := StripPrefix(id.Value, kataIDPrefix)
	if !ok {
		return sandboxruntime.ExecResult{}, fmt.Errorf("not a kata runtime id: %q", id.Value)
	}
	inst, err := k.loadInstance(name)
	if err != nil {
		return sandboxruntime.ExecResult{}, err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return execViaAgent(execCtx, inst.VsockPath, inst.ContainerID, req.Command)
}

// startVirtiofsDaemons launches virtiofsd for each planned share in find-paths
// migration mode. cache=always matches the read-only lower (substrate): the
// guest may cache it freely because no host-side writer can invalidate it.
// Daemons outlive the restore RPC.
func (k *Kata) startVirtiofsDaemons(ctx context.Context, vmDir string, shares []virtiofsShare) ([]*exec.Cmd, error) {
	var cmds []*exec.Cmd
	for i, share := range shares {
		if share.SharedDir == "" || share.Socket == "" {
			continue
		}
		if !dirExists(share.SharedDir) {
			killCmds(cmds)
			return nil, fmt.Errorf("virtiofs sharedDir %q missing for restore", share.SharedDir)
		}
		_ = os.Remove(share.Socket)
		logPath := filepath.Join(vmDir, fmt.Sprintf("virtiofsd-%d.log", i))
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			killCmds(cmds)
			return nil, fmt.Errorf("open virtiofsd log: %w", err)
		}
		// Not CommandContext: virtiofsd must outlive the LoadSnapshot RPC
		// (CH demand-pages / migrates fs state against it for the VM lifetime).
		cmd := exec.Command(k.VirtiofsdPath,
			"--socket-path="+share.Socket,
			"--shared-dir="+share.SharedDir,
			"--cache=always",
			"--thread-pool-size=1",
			"--announce-submounts",
			"--migration-mode", "find-paths",
			"--sandbox=none",
		)
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			_ = logFile.Close()
			killCmds(cmds)
			return nil, fmt.Errorf("start virtiofsd for %q: %w", share.SharedDir, err)
		}
		if err := waitSocketReady(ctx, share.Socket, 10*time.Second); err != nil {
			_ = cmd.Process.Kill()
			_ = logFile.Close()
			killCmds(cmds)
			return nil, fmt.Errorf("wait virtiofsd socket for %q: %w", share.SharedDir, err)
		}
		// virtiofsd holds its own dup for the VM's lifetime; ours would leak.
		_ = logFile.Close()
		cmds = append(cmds, cmd)
	}
	return cmds, nil
}

// restoreTapNet is one guest NIC whose tap file descriptors are passed into CH.
type restoreTapNet struct {
	id  string
	fds []int
}

func createRestoreTaps(slotID int32, devs []kataNetDevice) ([]restoreTapNet, []*os.File, error) {
	if err := ensureSharedBridge(context.Background()); err != nil {
		return nil, nil, err
	}
	ensureOutboundNAT(context.Background())

	var nets []restoreTapNet
	var files []*os.File
	for i, d := range devs {
		qp := d.QueuePairs
		if qp < 1 {
			qp = 1
		}
		// IFNAMSIZ=16; keep unique per slot so concurrent child restores do not
		// collide on sftap0 (substrate uses per-actor netns + fixed tap names).
		tapName := fmt.Sprintf("sft%d-%d", slotID, i)
		if len(tapName) > 15 {
			tapName = fmt.Sprintf("s%d-%d", slotID, i)
		}
		fds, opened, err := openTapDeviceFDs(tapName, qp)
		if err != nil {
			closeFiles(files)
			return nil, nil, err
		}
		if err := attachTapToBridge(tapName); err != nil {
			closeFiles(opened)
			closeFiles(files)
			return nil, nil, err
		}
		nets = append(nets, restoreTapNet{id: d.ID, fds: fds})
		files = append(files, opened...)
	}
	return nets, files, nil
}

func attachTapToBridge(tapName string) error {
	ctx := context.Background()
	if err := runIPCommand(ctx, "link", "set", tapName, "master", guestBridge); err != nil {
		return fmt.Errorf("tap %s master %s: %w", tapName, guestBridge, err)
	}
	if err := runIPCommand(ctx, "link", "set", tapName, "up"); err != nil {
		return fmt.Errorf("tap %s up: %w", tapName, err)
	}
	return nil
}

func openTapDeviceFDs(name string, queuePairs int) ([]int, []*os.File, error) {
	const (
		tunSetIFF     = 0x400454ca
		iffTap        = 0x0002
		iffNoPI       = 0x1000
		iffVnetHdr    = 0x4000 // IFF_VNET_HDR: CH virtio-net prepends virtio_net_hdr
		iffMultiQueue = 0x0100
	)
	if queuePairs < 1 {
		queuePairs = 1
	}
	var fds []int
	var files []*os.File
	for q := 0; q < queuePairs; q++ {
		f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
		if err != nil {
			closeFiles(files)
			return nil, nil, fmt.Errorf("open /dev/net/tun: %w", err)
		}
		var ifr struct {
			name  [16]byte
			flags uint16
			_     [22]byte
		}
		copy(ifr.name[:], name)
		// Match substrate setupRestoreTap: NO_PI + VNET_HDR; MULTI_QUEUE only when
		// more than one queue pair (otherwise CH/host frame parsing blackholes L2).
		ifr.flags = iffTap | iffNoPI | iffVnetHdr
		if queuePairs > 1 {
			ifr.flags |= iffMultiQueue
		}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), tunSetIFF, uintptr(unsafe.Pointer(&ifr)))
		if errno != 0 {
			f.Close()
			closeFiles(files)
			return nil, nil, errno
		}
		fds = append(fds, int(f.Fd()))
		files = append(files, f)
	}
	return fds, files, nil
}

func (c *chClient) restoreVMWithNetworkFDs(ctx context.Context, sourceDir string, nets []restoreTapNet, memMode string) error {
	type netCfg struct {
		ID     string `json:"id"`
		NumFDs int    `json:"num_fds"`
	}
	bodyObj := struct {
		SourceURL         string   `json:"source_url"`
		MemoryRestoreMode string   `json:"memory_restore_mode,omitempty"`
		NetFDs            []netCfg `json:"net_fds,omitempty"`
	}{SourceURL: "file://" + sourceDir, MemoryRestoreMode: memMode}
	var fds []int
	for _, n := range nets {
		bodyObj.NetFDs = append(bodyObj.NetFDs, netCfg{ID: n.id, NumFDs: len(n.fds)})
		fds = append(fds, n.fds...)
	}
	body, err := json.Marshal(bodyObj)
	if err != nil {
		return err
	}
	raddr, err := net.ResolveUnixAddr("unix", c.apiSocket)
	if err != nil {
		return err
	}
	conn, err := net.DialUnix("unix", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(kataVMRestoreTimeout))
	}
	req := fmt.Sprintf("PUT /api/v1/vm.restore HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	if _, _, err := conn.WriteMsgUnix([]byte(req), oob, nil); err != nil {
		return fmt.Errorf("send vm.restore: %w", err)
	}
	status, err := bufioReadStatus(conn)
	if err != nil {
		return err
	}
	if len(status) < 1 || status[0] != '2' {
		return fmt.Errorf("vm.restore failed: %s", strings.TrimSpace(status))
	}
	return nil
}

// withCHLog appends tails of cloud-hypervisor log files to err and prints them
// for Worker logs. paths may include --log-file output and a stderr capture.
func withCHLog(err error, flush *os.File, paths ...string) error {
	if err == nil {
		return nil
	}
	if flush != nil {
		_ = flush.Sync()
	}
	var parts []string
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			parts = append(parts, fmt.Sprintf("%s: <unreadable: %v>", filepath.Base(path), readErr))
			continue
		}
		if len(data) == 0 {
			parts = append(parts, fmt.Sprintf("%s: <empty>", filepath.Base(path)))
			continue
		}
		const maxTail = 8192
		if len(data) > maxTail {
			data = data[len(data)-maxTail:]
		}
		tail := strings.TrimSpace(string(data))
		parts = append(parts, fmt.Sprintf("%s:\n%s", filepath.Base(path), tail))
	}
	if len(parts) == 0 {
		return err
	}
	joined := strings.Join(parts, "\n---\n")
	log.Printf("cloud-hypervisor log tail:\n%s", joined)
	return fmt.Errorf("%w; cloud-hypervisor log tail:\n%s", err, joined)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

// placeSnapshotDir moves src to dst when possible (same filesystem), otherwise
// copies. Callers pass a disposable download directory as src.
func placeSnapshotDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	_ = os.RemoveAll(dst)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyDir(src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return nil
}

func killCmds(cmds []*exec.Cmd) {
	for _, c := range cmds {
		if c != nil && c.Process != nil {
			_ = c.Process.Kill()
		}
	}
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

func bufioReadStatus(conn *net.UnixConn) (string, error) {
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 {
		return line, nil
	}
	return parts[1], nil
}
