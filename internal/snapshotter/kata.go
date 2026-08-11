package snapshotter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

const (
	kataFormatVersion = "cloud-hypervisor-snapshot-v1"
	kataIDPrefix      = "kata:"
	kataMetaFile      = "sandboxfleet-meta.json"
)

// Kata memory-snapshots Cloud Hypervisor VMs (CRI parents and restored children).
//
// Save: pause → snapshot → resume; record AppImage + container id (gVisor/substrate-style).
// Writable files under DefaultFilesRoot ship as rootfs-upper.tar; RO base is rebuilt from
// the image at restore. Nested fork: SaveSnapshot also accepts restored ids ("kata:<name>").
type Kata struct {
	CloudHypervisorPath string
	VirtiofsdPath       string
	SocketSearchRoots   []string
	StateDir            string
}

type kataMeta struct {
	SourceSandboxID  string          `json:"sourceSandboxID"`
	ContainerID      string          `json:"containerID,omitempty"`
	AppImage         string          `json:"appImage,omitempty"`
	AppContainerName string          `json:"appContainerName,omitempty"`
	VirtiofsShares   []virtiofsShare `json:"virtiofsShares,omitempty"`
	NetDevices       []kataNetDevice `json:"netDevices,omitempty"`
	SavedAt          time.Time       `json:"savedAt"`
}

type virtiofsShare struct {
	Tag       string `json:"tag"`
	SharedDir string `json:"sharedDir,omitempty"` // save-time host path (optional hint)
	// UpperTar packs DefaultFilesRoot under <containerID>/rootfs (writable layer).
	UpperTar string `json:"upperTar,omitempty"`
	// RootfsTar is legacy (full share archive); ignored when AppImage is set.
	RootfsTar string `json:"rootfsTar,omitempty"`
	// Socket is set only when planning a restore (not required in saved meta).
	Socket string `json:"socket,omitempty"`
}

type kataNetDevice struct {
	ID         string `json:"id"`
	QueuePairs int    `json:"queuePairs"`
}

func NewKata() *Kata {
	return &Kata{
		// Prefer restore-only binaries under /opt/sandboxfleet (CH v52 +
		// virtiofsd 1.14). Fall back to kata-static paths for local/dev images
		// that have not installed the SandboxFleet overlay.
		CloudHypervisorPath: firstNonEmpty(
			os.Getenv("SANDBOXFLEET_CLOUD_HYPERVISOR_PATH"),
			"/opt/sandboxfleet/bin/cloud-hypervisor",
			"/opt/kata/bin/cloud-hypervisor",
			"cloud-hypervisor",
		),
		VirtiofsdPath: firstExistingPath(
			os.Getenv("SANDBOXFLEET_VIRTIOFSD_PATH"),
			"/opt/sandboxfleet/bin/virtiofsd",
			"/opt/kata/libexec/virtiofsd",
			"/opt/kata/bin/virtiofsd",
			"virtiofsd",
		),
		SocketSearchRoots: []string{"/run/vc/vm", "/run/vc/sbs"},
		StateDir:          firstNonEmpty(os.Getenv("SANDBOXFLEET_KATA_STATE_DIR"), "/var/lib/sandboxfleet/kata"),
	}
}

func (k *Kata) FormatVersion() string { return kataFormatVersion }

func (k *Kata) SaveSnapshot(ctx context.Context, req SaveRequest) error {
	if req.ID.Value == "" {
		return fmt.Errorf("runtime id is required")
	}
	// Restored (nested-fork) instances are tracked in StateDir, not under CRI.
	if strings.HasPrefix(req.ID.Value, kataIDPrefix) {
		return k.saveRestoredVMSnapshot(ctx, req)
	}

	vmDir, apiSocket, err := k.findVM(req.ID.Value)
	if err != nil {
		return err
	}
	return k.saveCHSnapshot(ctx, req, vmDir, apiSocket, req.ContainerID)
}

func (k *Kata) saveCHSnapshot(ctx context.Context, req SaveRequest, vmDir, apiSocket, containerID string) error {
	if err := os.MkdirAll(req.DestDir, 0o755); err != nil {
		return err
	}

	shares := discoverVirtiofsShares(vmDir)
	if len(shares) == 0 {
		return fmt.Errorf("no virtiofs sharedDir found for sandbox %q (required for memory restore)", filepath.Base(vmDir))
	}

	client := newCHClient(apiSocket)
	if err := client.Pause(ctx); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	snapshotErr := client.Snapshot(ctx, req.DestDir)
	// Resume immediately after snapshot. Packing rootfs while still paused can
	// block the CLH API long enough for kata's monitor (vmm.ping ~10s) to declare
	// the hypervisor dead and tear down the VM.
	resumeErr := client.Resume(ctx)
	if snapshotErr != nil {
		if resumeErr != nil {
			return fmt.Errorf("snapshot vm: %v; resume: %w", snapshotErr, resumeErr)
		}
		return fmt.Errorf("snapshot vm: %w", snapshotErr)
	}
	if resumeErr != nil {
		return fmt.Errorf("resume vm after snapshot: %w", resumeErr)
	}

	// Substrate/gVisor-style: RO rootfs is rebuilt from AppImage at restore; only pack
	// the writable layer (DefaultFilesRoot) for cross-Worker file persistence.
	for i := range shares {
		if containerID == "" || req.AppImage == "" || !isKataSharedTag(shares[i].Tag) {
			continue
		}
		upperRel := strings.TrimPrefix(sandboxruntime.DefaultFilesRoot, "/")
		upperSrc := filepath.Join(shares[i].SharedDir, containerID, "rootfs", upperRel)
		if !dirExists(upperSrc) {
			continue
		}
		name := rootfsUpperTarFileName()
		if err := packRootfsTar(upperSrc, filepath.Join(req.DestDir, name)); err != nil {
			return fmt.Errorf("archive rootfs upper %q: %w", upperSrc, err)
		}
		shares[i].UpperTar = name
	}

	meta := kataMeta{
		SourceSandboxID:  req.ID.Value,
		ContainerID:      containerID,
		AppImage:         req.AppImage,
		AppContainerName: req.AppContainerName,
		VirtiofsShares:   shares,
		NetDevices:       readNetDevicesFromConfig(filepath.Join(req.DestDir, "config.json")),
		SavedAt:          time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(req.DestDir, kataMetaFile), raw, 0o600)
}

func (k *Kata) findVM(sandboxID string) (vmDir, apiSocket string, err error) {
	// Only clh-api.sock speaks Cloud Hypervisor HTTP (vm.pause / vm.snapshot).
	// clh.sock is the guest vsock used by kata-agent — never use it as the API.
	const apiSockName = "clh-api.sock"
	candidates := []string{
		filepath.Join("/run/vc/vm", sandboxID, apiSockName),
		filepath.Join("/run/vc/sbs", sandboxID, apiSockName),
	}
	for _, root := range k.SocketSearchRoots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			if d.Name() == apiSockName && strings.Contains(path, sandboxID) {
				candidates = append([]string{path}, candidates...)
			}
			return nil
		})
	}
	for _, path := range candidates {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return filepath.Dir(path), path, nil
		}
	}
	return "", "", fmt.Errorf("cloud-hypervisor api socket (%s) not found for sandbox %q", apiSockName, sandboxID)
}

func discoverVirtiofsShares(vmDir string) []virtiofsShare {
	sandboxID := filepath.Base(vmDir)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fallbackKataSharedDir(sandboxID)
	}
	var out []virtiofsShare
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) == 0 || !strings.Contains(args[0], "virtiofsd") {
			continue
		}
		// Kata virtiofsd often uses --fd=3 (no socket path under vmDir) and
		// --shared-dir=/run/kata-containers/shared/sandboxes/<id>/shared.
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, vmDir) && !strings.Contains(joined, sandboxID) {
			continue
		}
		share := virtiofsShare{}
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case strings.HasPrefix(a, "--tag="):
				share.Tag = strings.TrimPrefix(a, "--tag=")
			case a == "--tag" && i+1 < len(args):
				share.Tag = args[i+1]
			case strings.HasPrefix(a, "--shared-dir="):
				share.SharedDir = strings.TrimPrefix(a, "--shared-dir=")
			case a == "--shared-dir" && i+1 < len(args):
				share.SharedDir = args[i+1]
			}
		}
		if !dirExists(share.SharedDir) {
			continue
		}
		if share.Tag == "" {
			share.Tag = "kataShared"
		}
		out = append(out, share)
	}
	if len(out) == 0 {
		return fallbackKataSharedDir(sandboxID)
	}
	return dedupeVirtiofsShares(out)
}

// dedupeVirtiofsShares drops duplicate SharedDir entries (discover can see
// more than one virtiofsd cmdline match for the same sandbox share).
func dedupeVirtiofsShares(shares []virtiofsShare) []virtiofsShare {
	if len(shares) < 2 {
		return shares
	}
	seen := make(map[string]struct{}, len(shares))
	out := make([]virtiofsShare, 0, len(shares))
	for _, s := range shares {
		key := s.SharedDir
		if key == "" {
			out = append(out, s)
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

// fallbackKataSharedDir uses the conventional Kata host share when virtiofsd
// was started with --fd= (cmdline may not include vmDir).
func fallbackKataSharedDir(sandboxID string) []virtiofsShare {
	if sandboxID == "" || sandboxID == "." || sandboxID == "/" {
		return nil
	}
	shared := filepath.Join("/run/kata-containers/shared/sandboxes", sandboxID, "shared")
	if st, err := os.Stat(shared); err != nil || !st.IsDir() {
		return nil
	}
	return []virtiofsShare{{Tag: "kataShared", SharedDir: shared}}
}

func readNetDevicesFromConfig(configPath string) []kataNetDevice {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	nets, _ := cfg["net"].([]any)
	var out []kataNetDevice
	for _, n := range nets {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		qp := 1
		if v, ok := m["num_queues"].(float64); ok && int(v) > 0 {
			qp = int(v) / 2
			if qp < 1 {
				qp = 1
			}
		}
		out = append(out, kataNetDevice{ID: id, QueuePairs: qp})
	}
	return out
}

func readKataMeta(dir string) (kataMeta, error) {
	raw, err := os.ReadFile(filepath.Join(dir, kataMetaFile))
	if err != nil {
		return kataMeta{}, err
	}
	var meta kataMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return kataMeta{}, err
	}
	return meta, nil
}

func isKataSharedTag(tag string) bool {
	return tag == "" || tag == "kataShared"
}
