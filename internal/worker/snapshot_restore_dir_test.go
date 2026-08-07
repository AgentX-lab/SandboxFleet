package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/snapshotter"
)

type recordingSnapshotter struct {
	deleted []string
}

func (r *recordingSnapshotter) FormatVersion() string { return "test" }

func (r *recordingSnapshotter) SaveSnapshot(context.Context, snapshotter.SaveRequest) error {
	return nil
}

func (r *recordingSnapshotter) LoadSnapshot(_ context.Context, _ snapshotter.LoadRequest) (sandboxruntime.ID, error) {
	return sandboxruntime.ID{Value: "runsc:test"}, nil
}

func (r *recordingSnapshotter) DeleteRestored(_ context.Context, id sandboxruntime.ID) error {
	r.deleted = append(r.deleted, id.Value)
	return nil
}

func (r *recordingSnapshotter) ExecRestored(context.Context, sandboxruntime.ID, sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	return sandboxruntime.ExecResult{}, nil
}

func TestReleaseSlotClearsRestoreDir(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "checkpoint")
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatalf("mkdir checkpoint: %v", err)
	}

	snap := &recordingSnapshotter{}
	reg := snapshotter.NewRegistry(map[string]snapshotter.Snapshotter{"runsc": snap})
	manager := NewSlotManager(Config{
		Slots: []slot.Config{{ID: 0, Profile: "default"}},
		Runtime: sandboxv1alpha1.RuntimeConfig{
			Backend: sandboxv1alpha1.RuntimeBackendCRI,
			CRI:     &sandboxv1alpha1.CRIRuntimeConfig{RuntimeHandler: "runsc", Snapshotter: sandboxv1alpha1.SnapshotterGVisor},
		},
	}, newFakeRuntime(), reg)

	identity := SandboxIdentity{Namespace: "ns", Name: "child", UID: "uid-1"}
	current := manager.slots[0]
	current.state = slot.StateRunning
	current.sandbox = identity
	id := sandboxruntime.ID{Value: "runsc:child"}
	current.runtimeRef = &id
	current.restored = true
	current.restoreDir = dir

	if err := manager.ReleaseSlot(context.Background(), SandboxSlotRef{SlotID: 0, Identity: identity}); err != nil {
		t.Fatalf("ReleaseSlot: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("restore dir still present: %v", err)
	}
	if current.restoreDir != "" {
		t.Fatalf("restoreDir = %q, want empty", current.restoreDir)
	}
	if len(snap.deleted) != 1 || snap.deleted[0] != "runsc:child" {
		t.Fatalf("DeleteRestored calls = %#v, want [runsc:child]", snap.deleted)
	}
}

func TestStopSandboxClearsRestoreDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	snap := &recordingSnapshotter{}
	reg := snapshotter.NewRegistry(map[string]snapshotter.Snapshotter{"runsc": snap})
	manager := NewSlotManager(Config{
		Slots: []slot.Config{{ID: 0, Profile: "default"}},
		Runtime: sandboxv1alpha1.RuntimeConfig{
			Backend: sandboxv1alpha1.RuntimeBackendCRI,
			CRI:     &sandboxv1alpha1.CRIRuntimeConfig{RuntimeHandler: "runsc", Snapshotter: sandboxv1alpha1.SnapshotterGVisor},
		},
	}, newFakeRuntime(), reg)

	identity := SandboxIdentity{Namespace: "ns", Name: "child", UID: "uid-2"}
	current := manager.slots[0]
	current.state = slot.StateRunning
	current.sandbox = identity
	id := sandboxruntime.ID{Value: "runsc:child-2"}
	current.runtimeRef = &id
	current.restored = true
	current.restoreDir = dir

	if err := manager.StopSandbox(context.Background(), SandboxSlotRef{SlotID: 0, Identity: identity}); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("restore dir still present after stop: %v", err)
	}
	if current.restoreDir != "" {
		t.Fatalf("restoreDir = %q, want empty", current.restoreDir)
	}
}
