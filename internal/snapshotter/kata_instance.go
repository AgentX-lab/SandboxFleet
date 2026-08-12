package snapshotter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// kataInstance is the on-disk record of one self-managed micro-VM (sockets, pid,
// container ids, image bundle). It is the only handle the Worker has on a kata
// sandbox: exec, checkpoint and teardown all go through it.
type kataInstance struct {
	// Name is the instance key (RestoredName), i.e. the id suffix after "kata:".
	Name        string `json:"name"`
	Namespace   string `json:"namespace,omitempty"`
	SandboxName string `json:"sandboxName,omitempty"`
	UID         string `json:"uid,omitempty"`
	SlotID      int32  `json:"slotID"`

	VMDir     string `json:"vmDir"`
	APISocket string `json:"apiSocket"`
	VsockPath string `json:"vsockPath"`
	// SnapshotDir is set for restored instances (CH's restore source).
	SnapshotDir string `json:"snapshotDir,omitempty"`
	// ContainerID is the overlay workload the agent runs (<carrier>_ovl); exec
	// and checkpoint meta both key on it.
	ContainerID string `json:"containerID"`
	// BaseID is the frozen sandbox id the guest's virtio-fs paths are pinned to.
	BaseID string `json:"baseID,omitempty"`
	// AppImage is the workload image, re-unpacked when this VM is restored.
	AppImage string `json:"appImage,omitempty"`
	// BundleDir holds the host-side overlay rootfs backing the RO lower.
	BundleDir string `json:"bundleDir,omitempty"`
	PID       int    `json:"pid"`
}

// Instance is the exported view of a self-managed micro-VM, for the kata
// Runtime adapter (List / Status / PrimaryContainerID).
type Instance struct {
	ID          sandboxruntime.ID
	Identity    sandboxruntime.SandboxIdentity
	SlotID      int32
	ContainerID string
	PID         int
}

// Instance returns the record behind a "kata:<name>" runtime id.
func (k *Kata) Instance(id sandboxruntime.ID) (Instance, error) {
	name, ok := StripPrefix(id.Value, kataIDPrefix)
	if !ok {
		return Instance{}, fmt.Errorf("not a kata runtime id: %q", id.Value)
	}
	inst, err := k.loadInstance(name)
	if err != nil {
		if os.IsNotExist(err) {
			return Instance{}, fmt.Errorf("%w: kata instance %q", sandboxruntime.ErrNotFound, name)
		}
		return Instance{}, err
	}
	return inst.exported(), nil
}

// Instances lists every micro-VM this Worker still tracks, for slot recovery.
func (k *Kata) Instances() ([]Instance, error) {
	entries, err := os.ReadDir(k.instancesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	out := make([]Instance, 0, len(names))
	for _, name := range names {
		inst, err := k.loadInstance(name)
		if err != nil {
			continue
		}
		out = append(out, inst.exported())
	}
	return out, nil
}

func (i kataInstance) exported() Instance {
	return Instance{
		ID: sandboxruntime.ID{Value: kataIDPrefix + i.Name},
		Identity: sandboxruntime.SandboxIdentity{
			Namespace: i.Namespace,
			Name:      i.SandboxName,
			UID:       types.UID(i.UID),
		},
		SlotID:      i.SlotID,
		ContainerID: i.ContainerID,
		PID:         i.PID,
	}
}

func (k *Kata) instancesDir() string { return filepath.Join(k.StateDir, "instances") }

func (k *Kata) instancePath(name string) string {
	return filepath.Join(k.instancesDir(), name+".json")
}

// bundleDir is where a micro-VM's host-side overlay rootfs (the virtio-fs RO
// lower) is composed from the workload image.
func (k *Kata) bundleDir(name string) string {
	return filepath.Join(k.StateDir, "bundles", name)
}

func (k *Kata) saveInstance(inst kataInstance) error {
	if err := os.MkdirAll(k.instancesDir(), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	return os.WriteFile(k.instancePath(inst.Name), raw, 0o600)
}

func (k *Kata) loadInstance(name string) (kataInstance, error) {
	raw, err := os.ReadFile(k.instancePath(name))
	if err != nil {
		return kataInstance{}, err
	}
	var inst kataInstance
	return inst, json.Unmarshal(raw, &inst)
}
