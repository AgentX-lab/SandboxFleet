package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
)

func TestSandboxLifecycle(t *testing.T) {
	ctx := context.Background()
	runtime := newFakeRuntime()
	manager := NewSlotManager(Config{Slots: 1}, runtime)
	identity := SandboxIdentity{Namespace: "ns", Name: "sandbox", UID: "uid-1"}
	ref := SandboxSlotRef{SlotID: 0, Identity: identity}

	if err := manager.ReserveSlot(ctx, ref); err != nil {
		t.Fatalf("ReserveSlot() error = %v", err)
	}
	if err := manager.ReserveSlot(ctx, ref); err != nil {
		t.Fatalf("idempotent ReserveSlot() error = %v", err)
	}
	if err := manager.StartSandbox(ctx, StartSandboxRequest{
		SlotID:    0,
		Identity:  identity,
		Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
	}); err != nil {
		t.Fatalf("StartSandbox() error = %v", err)
	}

	info, err := manager.GetSandbox(ctx, ref)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if info.State != slot.StateRunning {
		t.Fatalf("GetSandbox().State = %q, want Running", info.State)
	}

	execResult, err := manager.ExecSandbox(ctx, ExecSandboxRequest{
		SlotID:   0,
		Identity: identity,
		Command:  []string{"echo", "hi"},
	})
	if err != nil {
		t.Fatalf("ExecSandbox() error = %v", err)
	}
	if execResult.ExitCode != 0 || execResult.Stdout != "ran:echo" {
		t.Fatalf("ExecSandbox() = %#v, want exit 0 stdout ran:echo", execResult)
	}

	if err := manager.StopSandbox(ctx, ref); err != nil {
		t.Fatalf("StopSandbox() error = %v", err)
	}
	if err := manager.ReleaseSlot(ctx, ref); err != nil {
		t.Fatalf("ReleaseSlot() error = %v", err)
	}
	if got := manager.ListSlots(ctx)[0]; got.State != slot.StateFree || got.SandboxUID != "" {
		t.Fatalf("released slot = %#v, want Free without owner", got)
	}
}

func TestReserveRejectsAnotherSandbox(t *testing.T) {
	manager := NewSlotManager(Config{Slots: 1}, newFakeRuntime())
	first := SandboxSlotRef{SlotID: 0, Identity: SandboxIdentity{Namespace: "ns", Name: "first", UID: "uid-1"}}
	second := SandboxSlotRef{SlotID: 0, Identity: SandboxIdentity{Namespace: "ns", Name: "second", UID: "uid-2"}}

	if err := manager.ReserveSlot(context.Background(), first); err != nil {
		t.Fatalf("ReserveSlot() error = %v", err)
	}
	if err := manager.ReserveSlot(context.Background(), second); err == nil {
		t.Fatal("ReserveSlot() error = nil, want conflict")
	}
}

func TestRecoverRebuildsSlotState(t *testing.T) {
	runtime := newFakeRuntime()
	runtime.objects["runtime-1"] = sandboxruntime.Info{
		ID:       sandboxruntime.ID{Value: "runtime-1"},
		Identity: sandboxruntime.SandboxIdentity{Namespace: "ns", Name: "sandbox", UID: "uid-1"},
		SlotID:   0,
		Status:   sandboxruntime.Status{State: sandboxruntime.StateRunning},
	}
	manager := NewSlotManager(Config{Slots: 1}, runtime)

	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if got := manager.ListSlots(context.Background())[0]; got.State != slot.StateRunning || got.SandboxUID != "uid-1" {
		t.Fatalf("recovered slot = %#v, want Running owned by uid-1", got)
	}
}

func TestStartRecreatesMissingRuntime(t *testing.T) {
	ctx := context.Background()
	runtime := newFakeRuntime()
	manager := NewSlotManager(Config{Slots: 1}, runtime)
	identity := SandboxIdentity{Namespace: "ns", Name: "sandbox", UID: "uid-1"}
	ref := SandboxSlotRef{SlotID: 0, Identity: identity}
	request := StartSandboxRequest{
		SlotID:    0,
		Identity:  identity,
		Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
	}

	if err := manager.ReserveSlot(ctx, ref); err != nil {
		t.Fatalf("ReserveSlot() error = %v", err)
	}
	if err := manager.StartSandbox(ctx, request); err != nil {
		t.Fatalf("first StartSandbox() error = %v", err)
	}
	clear(runtime.objects)
	if err := manager.StartSandbox(ctx, request); err != nil {
		t.Fatalf("second StartSandbox() error = %v", err)
	}
	if runtime.nextID != 2 {
		t.Fatalf("runtime creations = %d, want 2", runtime.nextID)
	}
}

type fakeRuntime struct {
	nextID  int
	objects map[string]sandboxruntime.Info
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{objects: make(map[string]sandboxruntime.Info)}
}

func (f *fakeRuntime) Create(_ context.Context, req sandboxruntime.CreateRequest) (sandboxruntime.ID, error) {
	f.nextID++
	id := sandboxruntime.ID{Value: fmt.Sprintf("runtime-%d", f.nextID)}
	f.objects[id.Value] = sandboxruntime.Info{
		ID:       id,
		Identity: req.Identity,
		SlotID:   req.SlotID,
		Status:   sandboxruntime.Status{State: sandboxruntime.StateCreated},
	}
	return id, nil
}

func (f *fakeRuntime) Start(_ context.Context, id sandboxruntime.ID) error {
	object := f.objects[id.Value]
	object.Status.State = sandboxruntime.StateRunning
	f.objects[id.Value] = object
	return nil
}

func (f *fakeRuntime) Stop(_ context.Context, id sandboxruntime.ID) error {
	object := f.objects[id.Value]
	object.Status.State = sandboxruntime.StateStopped
	f.objects[id.Value] = object
	return nil
}

func (f *fakeRuntime) Delete(_ context.Context, id sandboxruntime.ID) error {
	delete(f.objects, id.Value)
	return nil
}

func (f *fakeRuntime) Status(_ context.Context, id sandboxruntime.ID) (sandboxruntime.Status, error) {
	object, found := f.objects[id.Value]
	if !found {
		return sandboxruntime.Status{}, sandboxruntime.ErrNotFound
	}
	return object.Status, nil
}

func (f *fakeRuntime) List(context.Context) ([]sandboxruntime.Info, error) {
	result := make([]sandboxruntime.Info, 0, len(f.objects))
	for _, object := range f.objects {
		result = append(result, object)
	}
	return result, nil
}

func (f *fakeRuntime) Exec(_ context.Context, id sandboxruntime.ID, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	object, found := f.objects[id.Value]
	if !found {
		return sandboxruntime.ExecResult{}, sandboxruntime.ErrNotFound
	}
	if object.Status.State != sandboxruntime.StateRunning {
		return sandboxruntime.ExecResult{}, fmt.Errorf("runtime %q is not running", id.Value)
	}
	if len(req.Command) == 0 {
		return sandboxruntime.ExecResult{}, errors.New("command is required")
	}
	return sandboxruntime.ExecResult{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("ran:%s", req.Command[0]),
	}, nil
}
