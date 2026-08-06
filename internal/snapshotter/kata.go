package snapshotter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	kataFormatVersion = "cloud-hypervisor-snapshot-v1"
	kataIDPrefix      = "kata:"
	kataMetaFile      = "sandboxfleet-meta.json"
)

// Kata memory-snapshots Cloud Hypervisor VMs (CRI parents and restored children).
//
// Save: pause → snapshot → resume on the live CH api-socket.
// Load: relaunch CH, OnDemand restore, agent dial + guest networking, then Exec via ttrpc.
// Nested fork: SaveSnapshot also accepts restored ids ("kata:<name>") from StateDir.
type Kata struct {
	CloudHypervisorPath string
	VirtiofsdPath       string
	SocketSearchRoots   []string
	StateDir            string
}

type kataMeta struct {
	SourceSandboxID string          `json:"sourceSandboxID"`
	ContainerID     string          `json:"containerID,omitempty"`
	VirtiofsShares  []virtiofsShare `json:"virtiofsShares,omitempty"`
	NetDevices      []kataNetDevice `json:"netDevices,omitempty"`
	SavedAt         time.Time       `json:"savedAt"`
}

type virtiofsShare struct {
	Tag       string `json:"tag"`
	SharedDir string `json:"sharedDir"`
}

type kataNetDevice struct {
	ID         string `json:"id"`
	QueuePairs int    `json:"queuePairs"`
}

func NewKata() *Kata {
	return &Kata{
		CloudHypervisorPath: firstNonEmpty(os.Getenv("SANDBOXFLEET_CLOUD_HYPERVISOR_PATH"), "/opt/kata/bin/cloud-hypervisor", "cloud-hypervisor"),
		VirtiofsdPath:       firstNonEmpty(os.Getenv("SANDBOXFLEET_VIRTIOFSD_PATH"), "/opt/kata/bin/virtiofsd", "virtiofsd"),
		SocketSearchRoots:   []string{"/run/vc/vm", "/run/vc/sbs"},
		StateDir:            firstNonEmpty(os.Getenv("SANDBOXFLEET_KATA_STATE_DIR"), "/var/lib/sandboxfleet/kata"),
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

	meta := kataMeta{
		SourceSandboxID: req.ID.Value,
		ContainerID:     containerID,
		VirtiofsShares:  discoverVirtiofsShares(vmDir),
		SavedAt:         time.Now().UTC(),
	}

	client := newCHClient(apiSocket)
	if err := client.Pause(ctx); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	snapshotErr := client.Snapshot(ctx, req.DestDir)
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

	meta.NetDevices = readNetDevicesFromConfig(filepath.Join(req.DestDir, "config.json"))
	if meta.VirtiofsShares == nil {
		meta.VirtiofsShares = []virtiofsShare{}
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(req.DestDir, kataMetaFile), raw, 0o600)
}

func (k *Kata) findVM(sandboxID string) (vmDir, apiSocket string, err error) {
	candidates := []string{
		filepath.Join("/run/vc/vm", sandboxID, "clh-api.sock"),
		filepath.Join("/run/vc/vm", sandboxID, "clh.sock"),
		filepath.Join("/run/vc/sbs", sandboxID, "clh-api.sock"),
		filepath.Join("/run/vc/sbs", sandboxID, "clh.sock"),
	}
	for _, root := range k.SocketSearchRoots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			base := d.Name()
			if (base == "clh-api.sock" || base == "clh.sock") && strings.Contains(path, sandboxID) {
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
	return "", "", fmt.Errorf("cloud-hypervisor api socket not found for sandbox %q", sandboxID)
}

func discoverVirtiofsShares(vmDir string) []virtiofsShare {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
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
		if !strings.Contains(strings.Join(args, " "), vmDir) {
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
		if share.SharedDir != "" {
			out = append(out, share)
		}
	}
	return out
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
