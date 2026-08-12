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
	// kataBaseIDFile records the frozen sandbox id the guest's virtio-fs
	// find-paths are pinned to (<baseID>/rootfs). It is invariant across a
	// sandbox's restore lineage, so restore can lay the reconstructed RO lower
	// where the guest still expects it.
	kataBaseIDFile = "base-id"
	// kataOverlaySuffix separates a carrier container id from its overlay
	// workload id. "_" is invalid in a Kubernetes container name, so a workload
	// id can never collide with a carrier id.
	kataOverlaySuffix = "_ovl"

	// Guest sizing fallbacks when the kata config carries none; 512MiB/1vCPU
	// matches the Fleet CI Worker memory budget.
	kataDefaultMemoryMiB = 512
	kataDefaultVCPUs     = 1

	// Guest assets for a cold boot. SANDBOXFLEET_KATA_{KERNEL,IMAGE,CONFIG}
	// override them on images that stage kata-static elsewhere.
	defaultKataKernelPath = "/opt/kata/share/kata-containers/vmlinux.container"
	defaultKataImagePath  = "/opt/kata/share/kata-containers/kata-containers.img"
	defaultKataConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-clh.toml"
)

// Kata runs self-managed Cloud Hypervisor micro-VMs, aligned with substrate's
// ateom-microvm: the Worker boots CH itself (no kata shim, no CRI) and drives
// the stock kata-agent over hybrid-vsock to assemble each container's rootfs as
// overlay(OCI image read-only over virtio-fs + guest tmpfs upper).
//
// Because the writable upper is guest RAM, rootfs writes ride along in the
// memory snapshot and nothing rootfs-related ships:
//   - ColdBoot: virtiofsd + CH boot + agent CreateSandbox/carrier/workload
//   - SaveSnapshot: pause → snapshot → tear the VMM down (no resume)
//   - LoadSnapshot: rebuild the RO lower from the image, relaunch CH, resume
type Kata struct {
	CloudHypervisorPath string
	VirtiofsdPath       string
	StateDir            string
}

type kataMeta struct {
	SourceSandboxID  string `json:"sourceSandboxID"`
	ContainerID      string `json:"containerID,omitempty"`
	AppImage         string `json:"appImage,omitempty"`
	AppContainerName string `json:"appContainerName,omitempty"`
	// BaseID mirrors the base-id file; the file wins when both are present.
	BaseID         string          `json:"baseID,omitempty"`
	VirtiofsShares []virtiofsShare `json:"virtiofsShares,omitempty"`
	NetDevices     []kataNetDevice `json:"netDevices,omitempty"`
	SavedAt        time.Time       `json:"savedAt"`
}

type virtiofsShare struct {
	Tag       string `json:"tag"`
	SharedDir string `json:"sharedDir,omitempty"` // save-time host path (restore hint)
	// Socket is set only when planning a restore (not required in saved meta).
	Socket string `json:"socket,omitempty"`
}

type kataNetDevice struct {
	ID         string `json:"id"`
	QueuePairs int    `json:"queuePairs"`
}

func NewKata() *Kata {
	return &Kata{
		// Prefer the SandboxFleet overlay (CH v52 + virtiofsd 1.14, which speak
		// find-paths migration). Fall back to kata-static for dev images.
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
		StateDir: firstNonEmpty(os.Getenv("SANDBOXFLEET_KATA_STATE_DIR"), "/var/lib/sandboxfleet/kata"),
	}
}

func (k *Kata) FormatVersion() string { return kataFormatVersion }

// SaveSnapshot checkpoints a self-managed micro-VM. Only "kata:<name>" ids are
// accepted: there is no CRI-managed parent to discover any more.
func (k *Kata) SaveSnapshot(ctx context.Context, req SaveRequest) error {
	if req.ID.Value == "" {
		return fmt.Errorf("runtime id is required")
	}
	if !strings.HasPrefix(req.ID.Value, kataIDPrefix) {
		return fmt.Errorf("kata snapshotter only checkpoints self-managed ids (%q prefix), got %q", kataIDPrefix, req.ID.Value)
	}
	return k.saveSelfManagedSnapshot(ctx, req)
}

// TearsDownOnSave: the checkpoint ends with vm.shutdown, because a paused guest
// still pins its full memory allocation on the Worker.
func (k *Kata) TearsDownOnSave() bool { return true }

// kataAssetPaths returns the guest kernel, guest OS image and kata config paths.
func kataAssetPaths() (kernel, image, config string) {
	return firstNonEmpty(os.Getenv("SANDBOXFLEET_KATA_KERNEL"), defaultKataKernelPath),
		firstNonEmpty(os.Getenv("SANDBOXFLEET_KATA_IMAGE"), defaultKataImagePath),
		firstNonEmpty(os.Getenv("SANDBOXFLEET_KATA_CONFIG"), defaultKataConfigPath)
}

// kataOverlayWorkloadID is the kata container id of a carrier's overlay workload.
func kataOverlayWorkloadID(carrierID string) string { return carrierID + kataOverlaySuffix }

// kataCarrierID recovers the carrier container id (the virtio-fs <cid>/rootfs
// subdir) from saved meta.
func kataCarrierID(meta kataMeta) string {
	if meta.AppContainerName != "" {
		return meta.AppContainerName
	}
	return strings.TrimSuffix(meta.ContainerID, kataOverlaySuffix)
}

// readKataBaseID resolves the frozen base id for a staged snapshot dir. The
// base-id file wins over meta so snapshots written by older Workers still load.
func readKataBaseID(snapshotDir string, meta kataMeta) string {
	if raw, err := os.ReadFile(filepath.Join(snapshotDir, kataBaseIDFile)); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v
		}
	}
	return meta.BaseID
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
