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
	"golang.org/x/sys/unix"
)

// kataVMRestoreTimeout covers OnDemand restore of guest RAM on slow CI disks.
// Worker HTTP request contexts often have no deadline, so this is the real bound.
const kataVMRestoreTimeout = 5 * time.Minute

// restoreInstance is the on-disk record of a restored Kata child VM
// (sockets, pid, container id) used for Exec / Delete / nested snapshot.
type restoreInstance struct {
	Name        string `json:"name"`
	VMDir       string `json:"vmDir"`
	APISocket   string `json:"apiSocket"`
	VsockPath   string `json:"vsockPath"`
	SnapshotDir string `json:"snapshotDir"`
	ContainerID string `json:"containerID"`
	PID         int    `json:"pid"`
}

func (k *Kata) saveRestoredVMSnapshot(ctx context.Context, req SaveRequest) error {
	name, ok := StripPrefix(req.ID.Value, kataIDPrefix)
	if !ok {
		return fmt.Errorf("not a restored kata id: %q", req.ID.Value)
	}
	inst, err := k.loadRestoreInstance(name)
	if err != nil {
		return fmt.Errorf("read restored kata instance: %w", err)
	}
	containerID := req.ContainerID
	if containerID == "" {
		containerID = inst.ContainerID
	}
	if containerID == "" {
		return fmt.Errorf("restored kata instance %q has empty containerID", name)
	}
	return k.saveCHSnapshot(ctx, req, inst.VMDir, inst.APISocket, containerID)
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
	_ = unmountChildRootfs(vmDir)
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
	plannedFS, err := rewriteRestoreSockets(snapDir, vmDir, meta.VirtiofsShares)
	if err != nil {
		return sandboxruntime.ID{}, err
	}
	// Prefer RootfsTar unpack (cross-Worker); else bind live parent rootfs.
	plannedFS, err = k.prepareChildRootfsDirs(plannedFS, meta.SourceSandboxID, vmDir, snapDir)
	if err != nil {
		_ = os.RemoveAll(vmDir)
		return sandboxruntime.ID{}, err
	}

	vfsdCmds, err := k.startVirtiofsDaemons(ctx, vmDir, plannedFS)
	if err != nil {
		_ = unmountChildRootfs(vmDir)
		_ = os.RemoveAll(vmDir)
		return sandboxruntime.ID{}, err
	}

	nets, tapFiles, err := createRestoreTaps(req.SlotID, meta.NetDevices)
	if err != nil {
		killCmds(vfsdCmds)
		return sandboxruntime.ID{}, err
	}

	apiSocket := filepath.Join(vmDir, "clh-api.sock")
	vsockPath := filepath.Join(vmDir, "clh.sock")
	chLog, err := os.OpenFile(filepath.Join(vmDir, "cloud-hypervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		closeFiles(tapFiles)
		killCmds(vfsdCmds)
		return sandboxruntime.ID{}, fmt.Errorf("open cloud-hypervisor log: %w", err)
	}
	// Deliberately not CommandContext: VMM must outlive the restore RPC (substrate LaunchVMM).
	cmd := exec.Command(k.CloudHypervisorPath, "--api-socket", apiSocket)
	cmd.Stdout = chLog
	cmd.Stderr = chLog
	if err := cmd.Start(); err != nil {
		_ = chLog.Close()
		closeFiles(tapFiles)
		killCmds(vfsdCmds)
		return sandboxruntime.ID{}, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	client := newCHClient(apiSocket)
	chLogPath := filepath.Join(vmDir, "cloud-hypervisor.log")
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.WaitReady(waitCtx, 30*time.Second); err != nil {
		_ = cmd.Process.Kill()
		closeFiles(tapFiles)
		killCmds(vfsdCmds)
		_ = chLog.Sync()
		return sandboxruntime.ID{}, withCHLog(err, chLog, chLogPath)
	}
	// OnDemand matches substrate: faster restore; snapshot dir must stay for VM lifetime.
	// Use a dedicated deadline: parent ctx from Worker HTTP usually has none, and the
	// previous 60s default timed out on CI while loading ~1Gi memory-ranges.
	restoreCtx, restoreCancel := context.WithTimeout(ctx, kataVMRestoreTimeout)
	defer restoreCancel()
	if err := client.restoreVMWithNetworkFDs(restoreCtx, snapDir, nets, "OnDemand"); err != nil {
		_ = cmd.Process.Kill()
		closeFiles(tapFiles)
		killCmds(vfsdCmds)
		_ = chLog.Sync()
		return sandboxruntime.ID{}, withCHLog(fmt.Errorf("vm.restore: %w", err), chLog, chLogPath)
	}
	closeFiles(tapFiles)
	if err := client.Resume(ctx); err != nil {
		_ = cmd.Process.Kill()
		killCmds(vfsdCmds)
		_ = chLog.Sync()
		return sandboxruntime.ID{}, withCHLog(fmt.Errorf("resume restored vm: %w", err), chLog, chLogPath)
	}

	inst := restoreInstance{
		Name: name, VMDir: vmDir, APISocket: apiSocket, VsockPath: vsockPath,
		SnapshotDir: snapDir, ContainerID: meta.ContainerID, PID: cmd.Process.Pid,
	}
	if err := k.saveRestoreInstance(inst); err != nil {
		_ = client.Shutdown(ctx)
		_ = cmd.Process.Kill()
		killCmds(vfsdCmds)
		return sandboxruntime.ID{}, err
	}

	ac, err := dialAgentRetry(ctx, vsockPath, 45*time.Second)
	if err != nil {
		_ = k.DeleteRestored(ctx, sandboxruntime.ID{Value: kataIDPrefix + name})
		return sandboxruntime.ID{}, fmt.Errorf("wait kata-agent: %w", err)
	}
	// Best-effort guest networking so restored children can egress.
	// Address is unique per SlotID so nested fork / multi-child do not collide.
	_ = configureGuestNetwork(ctx, ac, req.SlotID)
	_ = ac.Close()

	return sandboxruntime.ID{Value: kataIDPrefix + name}, nil
}

func (k *Kata) DeleteRestored(ctx context.Context, id sandboxruntime.ID) error {
	name, ok := StripPrefix(id.Value, kataIDPrefix)
	if !ok {
		return fmt.Errorf("not a restored kata id: %q", id.Value)
	}
	inst, err := k.loadRestoreInstance(name)
	if err != nil {
		return nil
	}
	_ = newCHClient(inst.APISocket).Shutdown(ctx)
	if inst.PID > 0 {
		_ = unix.Kill(inst.PID, unix.SIGTERM)
	}
	_ = unmountChildRootfs(inst.VMDir)
	_ = os.RemoveAll(inst.VMDir)
	_ = os.Remove(k.restoreInstancePath(name))
	return nil
}

func (k *Kata) ExecRestored(ctx context.Context, id sandboxruntime.ID, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	name, ok := StripPrefix(id.Value, kataIDPrefix)
	if !ok {
		return sandboxruntime.ExecResult{}, fmt.Errorf("not a restored kata id: %q", id.Value)
	}
	inst, err := k.loadRestoreInstance(name)
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

func (k *Kata) restoreInstancePath(name string) string {
	return filepath.Join(k.StateDir, "instances", name+".json")
}

func (k *Kata) saveRestoreInstance(inst restoreInstance) error {
	if err := os.MkdirAll(filepath.Dir(k.restoreInstancePath(inst.Name)), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	return os.WriteFile(k.restoreInstancePath(inst.Name), raw, 0o600)
}

func (k *Kata) loadRestoreInstance(name string) (restoreInstance, error) {
	raw, err := os.ReadFile(k.restoreInstancePath(name))
	if err != nil {
		return restoreInstance{}, err
	}
	var inst restoreInstance
	return inst, json.Unmarshal(raw, &inst)
}

// startVirtiofsDaemons launches virtiofsd for each planned share (find-paths
// migration mode, aligned with substrate). Daemons outlive the restore RPC.
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
			"--cache=auto",
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
		cmds = append(cmds, cmd)
	}
	return cmds, nil
}

// restoreTapNet is one guest NIC whose tap file descriptors are passed into CH restore.
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
		iffMultiQueue = 0x0100
	)
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
		ifr.flags = iffTap | iffNoPI | iffMultiQueue
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

// withCHLog appends a tail of cloud-hypervisor.log to err and prints it for Worker logs.
func withCHLog(err error, chLog *os.File, path string) error {
	if err == nil {
		return nil
	}
	if chLog != nil {
		_ = chLog.Sync()
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || len(data) == 0 {
		return err
	}
	const maxTail = 4096
	if len(data) > maxTail {
		data = data[len(data)-maxTail:]
	}
	tail := strings.TrimSpace(string(data))
	log.Printf("cloud-hypervisor log tail (%s):\n%s", path, tail)
	return fmt.Errorf("%w; cloud-hypervisor log tail:\n%s", err, tail)
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
