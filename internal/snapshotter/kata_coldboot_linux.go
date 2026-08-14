//go:build linux

package snapshotter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/runtime/kata/ch"
	"github.com/AgentNaut/SandboxFleet/internal/runtime/kata/overlay"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// coldBootNetID is the CH device id of the guest's only virtio-net. Boot-time
// add-net does not take an id, but the snapshot's config.json does, and restore
// matches on it (createRestoreTaps reads it back from there).
const coldBootNetID = "_net0"

// coldBootAgentTimeout bounds the wait for the kata-agent: it only starts
// listening once the guest reaches kata-containers.target, which is the slowest
// step of a cold boot on a contended CI host.
const coldBootAgentTimeout = 90 * time.Second

// ColdBoot starts a sandbox as a self-managed micro-VM and returns its
// "kata:<name>" runtime id. Mirrors substrate coldBootActor: stage the RO lower
// from the OCI image, start virtiofsd, boot cloud-hypervisor with the kata guest
// kernel+image, then drive the kata-agent to create the sandbox and run the
// workload on an overlay rootfs.
func (k *Kata) ColdBoot(ctx context.Context, req sandboxruntime.CreateRequest) (id sandboxruntime.ID, retErr error) {
	if req.Container.Image == "" {
		return sandboxruntime.ID{}, fmt.Errorf("container image is required")
	}
	if len(req.Container.Command) == 0 && len(req.Container.Args) == 0 {
		return sandboxruntime.ID{}, fmt.Errorf("container command is required (image entrypoints are not resolved yet)")
	}
	name := RestoredName(req.Identity)
	baseID := kataSandboxID(req.Identity)
	carrierID := req.Identity.Name
	vmDir := overlay.VMDir(baseID)
	bundle := k.bundleDir(name)

	// A failed earlier attempt leaves sockets and binds behind under the same
	// (deterministic) id, which then collides on bind/mkdir.
	overlay.CleanupSandboxState(ctx, baseID)
	_ = os.Remove(k.instancePath(name))
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("create vm dir: %w", err)
	}
	defer func() {
		if retErr != nil {
			overlay.CleanupSandboxState(context.WithoutCancel(ctx), baseID)
			teardownImageRootfs(bundle)
			_ = os.RemoveAll(bundle)
		}
	}()

	bundleRootfs := filepath.Join(bundle, "rootfs")
	if err := setupImageRootfs(ctx, req.Container.Image, bundle); err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("compose rootfs for %q: %w", req.Container.Image, err)
	}
	// The guest gets no CreateSandbox.Dns and cluster DNS is unreachable from
	// sf-br0, so write public DNS into the lower before it is served read-only.
	if err := os.WriteFile(filepath.Join(bundleRootfs, "etc", "resolv.conf"), []byte(gvisorRestoreResolvConf), 0o644); err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("write guest resolv.conf: %w", err)
	}
	if err := overlay.ReconstructSharedDirFromImage(ctx, bundleRootfs, baseID, carrierID); err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("stage overlay lower: %w", err)
	}

	vfsdLog, err := os.OpenFile(filepath.Join(vmDir, "virtiofsd.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("open virtiofsd log: %w", err)
	}
	// The child keeps its own dup, so the Worker must not hold this for the life
	// of the VM — otherwise every cold boot leaks an fd.
	defer func() { _ = vfsdLog.Close() }()
	// Cache defaults to "always": the lower is remounted read-only, so there is
	// no host-side write churn for the guest page cache to miss.
	vfsdCmd, err := overlay.StartVirtiofsd(ctx, overlay.VirtiofsdOptions{
		Binary:     k.VirtiofsdPath,
		SocketPath: overlay.VirtiofsdSocketPath(baseID),
		SharedDir:  overlay.SharedDir(baseID),
		Log:        vfsdLog,
	})
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("start virtiofsd: %w", err)
	}
	defer func() {
		if retErr != nil {
			killCmds([]*exec.Cmd{vfsdCmd})
		}
	}()

	kernel, image, configPath := kataAssetPaths()
	cfgBytes, _ := os.ReadFile(configPath)
	cfg, err := overlay.ParseConfig(cfgBytes, kataDefaultMemoryMiB, kataDefaultVCPUs)
	if err != nil {
		return sandboxruntime.ID{}, err
	}
	kparams := overlay.WithDebugConsole(cfg.KernelParams)

	apiSocket := filepath.Join(vmDir, "clh-api.sock")
	chLog, err := os.OpenFile(filepath.Join(vmDir, "cloud-hypervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("open cloud-hypervisor log: %w", err)
	}
	defer func() { _ = chLog.Close() }()
	// The VMM must outlive this call, so ch.LaunchVMM deliberately does not bind
	// it to ctx; we own the process from here.
	chCmd, client, err := ch.LaunchVMM(ctx, ch.LaunchVMMOptions{
		Binary:    k.CloudHypervisorPath,
		APISocket: apiSocket,
		Stdout:    chLog,
		Stderr:    chLog,
	})
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("launch cloud-hypervisor: %w", err)
	}
	defer func() {
		if retErr != nil && chCmd.Process != nil {
			_ = chCmd.Process.Kill()
			_, _ = chCmd.Process.Wait()
		}
	}()

	serialLog := filepath.Join(vmDir, "serial.log")
	if err := client.CreateVM(ctx, kataBootVMConfig(baseID, kernel, image, kparams, serialLog, cfg.MemoryMiB, cfg.VCPUs)); err != nil {
		return sandboxruntime.ID{}, err
	}

	netCfg, err := guestNetForSlot(req.SlotID)
	if err != nil {
		return sandboxruntime.ID{}, err
	}
	nets, tapFiles, err := createRestoreTaps(req.SlotID, []kataNetDevice{{ID: coldBootNetID, QueuePairs: 1}})
	if err != nil {
		return sandboxruntime.ID{}, err
	}
	// CH dups the FDs it adopts, so ours always close.
	defer closeFiles(tapFiles)
	fds := nets[0].fds
	if err := client.AddNetWithFDs(ctx, netCfg.MAC, 2*len(fds), fds); err != nil {
		return sandboxruntime.ID{}, err
	}
	if err := client.BootVM(ctx); err != nil {
		return sandboxruntime.ID{}, err
	}

	vsockPath := overlay.VsockSocketPath(baseID)
	// CH creates the hybrid-vsock socket at boot; the agent only starts
	// answering CONNECT once the guest reaches kata-containers.target.
	if err := waitSocketReady(ctx, vsockPath, 30*time.Second); err != nil {
		logKataBootDiagnostics(baseID, serialLog, filepath.Join(vmDir, "virtiofsd.log"))
		return sandboxruntime.ID{}, fmt.Errorf("kata-agent vsock socket: %w", err)
	}
	ac, err := dialOverlayAgentRetry(ctx, vsockPath, coldBootAgentTimeout)
	if err != nil {
		logKataBootDiagnostics(baseID, serialLog, filepath.Join(vmDir, "virtiofsd.log"))
		return sandboxruntime.ID{}, fmt.Errorf("dial kata-agent: %w", err)
	}
	defer func() { _ = ac.Close() }()

	if err := ac.CreateSandboxForActor(ctx, baseID, carrierID, false); err != nil {
		return sandboxruntime.ID{}, err
	}
	if err := configureGuestNetwork(ctx, ac, req.SlotID); err != nil {
		logKataGuestNetworkDiag(ctx, vsockPath, filepath.Join(vmDir, "net.diag.txt"), req.SlotID, "cold boot "+baseID)
		return sandboxruntime.ID{}, fmt.Errorf("configure guest network: %w", err)
	}
	if err := writeKataBridgeDiag(ctx, filepath.Join(vmDir, "net.diag.txt"), req.SlotID); err != nil {
		log.Printf("kata cold boot %s: bridge diag: %v", baseID, err)
	}

	spec := kataWorkloadSpec(req)
	// The carrier materializes the RO base bind at /run/kata-containers/<cid>/rootfs,
	// which the workload's overlay then uses as its lowerdir.
	// Concurrent cold boots on one Worker can race virtiofs announce-submounts:
	// CreateCarrier briefly sees ENOENT for <cid>/rootfs until the submount is
	// visible — retry instead of failing the sandbox.
	if err := createCarrierWithRetry(ctx, ac, carrierID, spec); err != nil {
		logCarrierStagingDiag(baseID, carrierID, vmDir)
		return sandboxruntime.ID{}, err
	}
	workloadID := kataOverlayWorkloadID(carrierID)
	if err := ac.StartOverlayWorkload(ctx, carrierID, workloadID, overlay.OverlayUpperBase(carrierID), spec); err != nil {
		return sandboxruntime.ID{}, err
	}

	inst := kataInstance{
		Name:        name,
		Namespace:   req.Identity.Namespace,
		SandboxName: req.Identity.Name,
		UID:         string(req.Identity.UID),
		SlotID:      req.SlotID,
		VMDir:       vmDir,
		APISocket:   apiSocket,
		VsockPath:   vsockPath,
		ContainerID: workloadID,
		BaseID:      baseID,
		AppImage:    req.Container.Image,
		BundleDir:   bundle,
		PID:         chCmd.Process.Pid,
	}
	if err := k.saveInstance(inst); err != nil {
		return sandboxruntime.ID{}, err
	}
	log.Printf("kata cold boot %s: running (api=%s workload=%s)", name, apiSocket, workloadID)
	return sandboxruntime.ID{Value: kataIDPrefix + name}, nil
}

// kataSandboxID is the host-side sandbox id for a new micro-VM: the Sandbox UID,
// which is stable, unique per activation and safe as a path element.
func kataSandboxID(identity sandboxruntime.SandboxIdentity) string {
	if uid := string(identity.UID); uid != "" {
		return uid
	}
	return filepath.Base(identity.Name)
}

// kataBootVMConfig assembles the cloud-hypervisor VmConfig for a kata guest
// (substrate buildVMConfig). Beyond the kata clh cmdline it must set
// systemd.unit=kata-containers.target (else the guest powers off ~6s in) and
// mask systemd-networkd (the agent owns eth0). /dev/vda is the read-only guest
// image; the container rootfs lower is the virtio-fs device on PCI segment 1,
// hence num_pci_segments=2.
func kataBootVMConfig(id, kernel, image, kparams, serialLog string, memMiB, vcpus int) ch.VmConfig {
	console := "ttyS0"
	if runtime.GOARCH == "arm64" {
		console = "ttyAMA0"
	}
	cmdline := "root=/dev/vda1 rootflags=data=ordered,errors=remount-ro ro rootfstype=ext4 " +
		"panic=1 no_timer_check noreplace-smp console=" + console + ",115200n8 " +
		"systemd.unit=kata-containers.target systemd.mask=systemd-networkd.service systemd.mask=systemd-networkd.socket"
	if kparams != "" {
		cmdline += " " + kparams
	}
	return ch.VmConfig{
		Cpus: ch.CpusConfig{BootVcpus: int32(vcpus), MaxVcpus: int32(vcpus)},
		// Shared=true backs guest RAM with a memfd, which is what lets
		// vm.snapshot write a sparse memory image.
		Memory:  ch.MemoryConfig{Size: int64(memMiB) * 1024 * 1024, Shared: true},
		Payload: ch.PayloadConfig{Kernel: kernel, Cmdline: cmdline},
		Disks: []ch.DiskConfig{
			{Path: image, Readonly: true, ImageType: "Raw", NumQueues: int32(vcpus), QueueSize: 1024},
		},
		Fs: []ch.FsConfig{{
			Tag: overlay.FsTag, Socket: overlay.VirtiofsdSocketPath(id),
			NumQueues: 1, QueueSize: 1024, PciSegment: 1,
		}},
		Platform: &ch.PlatformConfig{NumPciSegments: 2},
		Rng:      &ch.RngConfig{Src: "/dev/urandom"},
		Serial:   &ch.ConsoleConfig{Mode: "File", File: serialLog},
		Vsock:    &ch.VsockConfig{Cid: 3, Socket: overlay.VsockSocketPath(id)},
	}
}

// kataWorkloadSpec builds the OCI spec the kata-agent needs (substrate
// ensureKataCompatibleSpec shape). Root.Path is a placeholder: CreateCarrier and
// StartOverlayWorkload overwrite it with the virtio-fs base and overlay mount.
func kataWorkloadSpec(req sandboxruntime.CreateRequest) *specs.Spec {
	args := append(append([]string(nil), req.Container.Command...), req.Container.Args...)
	env := []string{kataDefaultPATH, "HOME=/root", "TERM=xterm"}
	for _, v := range req.Container.Env {
		env = append(env, v.Name+"="+v.Value)
	}
	caps := defaultKataCapabilities()
	sandboxID := kataSandboxID(req.Identity)
	return &specs.Spec{
		Version:  specs.Version,
		Hostname: req.Identity.Name,
		Process: &specs.Process{
			Args:         args,
			Env:          env,
			Cwd:          "/",
			User:         specs.User{UID: 0, GID: 0},
			Capabilities: caps,
		},
		Root:   &specs.Root{Path: "rootfs"},
		Mounts: defaultKataMounts(),
		Linux: &specs.Linux{
			CgroupsPath: "/ateomchv/" + sandboxID,
			Resources:   defaultKataResources(),
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
				// Present so SpecToAgentPB drops it and the workload shares the
				// sandbox network (eth0 configured by configureGuestNetwork).
				// Matches substrate ensureKataCompatibleSpec.
				{Type: specs.NetworkNamespace},
			},
		},
	}
}

// defaultKataCapabilities mirrors the capability set containerd's kata CRI handler
// emits; the agent rejects CreateContainer without Process.Capabilities.
func defaultKataCapabilities() *specs.LinuxCapabilities {
	caps := []string{
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FSETID",
		"CAP_FOWNER",
		"CAP_MKNOD",
		"CAP_NET_RAW",
		"CAP_SETGID",
		"CAP_SETUID",
		"CAP_SETFCAP",
		"CAP_SETPCAP",
		"CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT",
		"CAP_KILL",
		"CAP_AUDIT_WRITE",
	}
	return &specs.LinuxCapabilities{
		Bounding:  caps,
		Effective: caps,
		Permitted: caps,
	}
}

// defaultKataMounts mirrors substrate defaultKataMounts (ctr run --runtime kata).
func defaultKataMounts() []specs.Mount {
	return []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
	}
}

// defaultKataResources mirrors substrate defaultKataResources.
func defaultKataResources() *specs.LinuxResources {
	dev := func(t string, major, minor int64, access string) specs.LinuxDeviceCgroup {
		d := specs.LinuxDeviceCgroup{Allow: true, Type: t, Access: access}
		if major != 0 {
			d.Major = &major
		}
		if minor >= 0 {
			d.Minor = &minor
		}
		return d
	}
	shares := uint64(1024)
	return &specs.LinuxResources{
		Devices: []specs.LinuxDeviceCgroup{
			{Allow: false, Access: "rwm"},
			dev("c", 1, 3, "rwm"),
			dev("c", 1, 8, "rwm"),
			dev("c", 1, 7, "rwm"),
			dev("c", 5, 0, "rwm"),
			dev("c", 1, 5, "rwm"),
			dev("c", 1, 9, "rwm"),
			dev("c", 5, 1, "rwm"),
			dev("c", 136, -1, "rwm"),
			dev("c", 5, 2, "rwm"),
		},
		CPU: &specs.LinuxCPU{Shares: &shares},
	}
}

const (
	// createCarrierAttempts covers virtiofs submount settle under concurrent
	// cold boots on a contended CI Worker (~3s worst case).
	createCarrierAttempts = 15
	createCarrierRetryGap = 200 * time.Millisecond
)

// createCarrierWithRetry calls CreateCarrier, retrying transient ENOENT while the
// guest virtiofs submount for <cid>/rootfs becomes visible.
func createCarrierWithRetry(ctx context.Context, ac *overlay.AgentClient, carrierID string, spec *specs.Spec) error {
	var last error
	for attempt := 1; attempt <= createCarrierAttempts; attempt++ {
		last = ac.CreateCarrier(ctx, carrierID, spec)
		if last == nil {
			if attempt > 1 {
				log.Printf("kata CreateCarrier %s: ok after %d attempts", carrierID, attempt)
			}
			return nil
		}
		if !kataErrLooksLikeENOENT(last) {
			return last
		}
		log.Printf("kata CreateCarrier %s: ENOENT attempt %d/%d: %v", carrierID, attempt, createCarrierAttempts, last)
		if attempt == createCarrierAttempts {
			break
		}
		timer := time.NewTimer(createCarrierRetryGap)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("create carrier %q: %w (last: %v)", carrierID, ctx.Err(), last)
		case <-timer.C:
		}
	}
	return last
}

// logCarrierStagingDiag dumps host-side shared-dir layout and virtiofsd log so
// CI artifacts explain CreateCarrier ENOENT after retries are exhausted.
func logCarrierStagingDiag(baseID, carrierID, vmDir string) {
	share := overlay.SharedDir(baseID)
	cmd := exec.Command("sh", "-c",
		fmt.Sprintf("echo '=== %s ==='; ls -la %q 2>&1; echo '=== %s/%s ==='; ls -laR %q 2>&1 | head -60",
			share, share, share, carrierID, filepath.Join(share, carrierID)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("kata CreateCarrier %s: host shared diag failed: %v (%s)", carrierID, err, strings.TrimSpace(string(out)))
	} else {
		log.Printf("kata CreateCarrier %s: host shared staging:\n%s", carrierID, strings.TrimSpace(string(out)))
	}
	vfsdLog := filepath.Join(vmDir, "virtiofsd.log")
	b, err := os.ReadFile(vfsdLog)
	if err != nil {
		log.Printf("kata CreateCarrier %s: read virtiofsd.log: %v", carrierID, err)
		return
	}
	const tail = 4096
	if len(b) > tail {
		b = b[len(b)-tail:]
	}
	log.Printf("kata CreateCarrier %s: virtiofsd.log tail:\n%s", carrierID, strings.TrimSpace(string(b)))
}

// dialOverlayAgentRetry polls DialAgent until the kata-agent answers the
// hybrid-vsock CONNECT. Callers wait for the socket first, so an ENOENT here
// means cloud-hypervisor unlinked it — the guest died and polling on would only
// burn the timeout to report "no such file".
func dialOverlayAgentRetry(ctx context.Context, vsockPath string, timeout time.Duration) (*overlay.AgentClient, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ac, err := overlay.DialAgent(dctx, vsockPath)
		cancel()
		if err == nil {
			return ac, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("guest stopped (cloud-hypervisor removed %q): %w", vsockPath, err)
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// logKataBootDiagnostics dumps what the host recorded about a guest that never
// reached the agent: the console tail (guest panic / early power-off) and
// virtiofsd's log (CH stops the VM when a vhost-user backend dies).
func logKataBootDiagnostics(id string, paths ...string) {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			continue
		}
		const maxTail = 4096
		if len(raw) > maxTail {
			raw = raw[len(raw)-maxTail:]
		}
		log.Printf("kata boot %s: %s tail:\n%s", id, filepath.Base(path), raw)
	}
}
