package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
)

func TestClientAndServerReserveSlot(t *testing.T) {
	manager := worker.NewSlotManager(worker.Config{Slots: 1}, &emptyRuntime{})
	server := httptest.NewServer(NewServer(manager))
	defer server.Close()

	client := NewClient(server.Client())
	ref := worker.SandboxSlotRef{
		SlotID:   0,
		Identity: worker.SandboxIdentity{Namespace: "ns", Name: "sandbox", UID: "uid-1"},
	}
	if err := client.ReserveSlot(context.Background(), server.URL, ref); err != nil {
		t.Fatalf("ReserveSlot() error = %v", err)
	}
	info, err := client.GetSandbox(context.Background(), server.URL, ref)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if info.Identity.UID != ref.Identity.UID {
		t.Fatalf("GetSandbox().UID = %q, want %q", info.Identity.UID, ref.Identity.UID)
	}
}

func TestClientAndServerExecSandbox(t *testing.T) {
	ctx := context.Background()
	manager := worker.NewSlotManager(worker.Config{Slots: 1}, &emptyRuntime{execStdout: "hello"})
	server := httptest.NewServer(NewServer(manager))
	defer server.Close()

	client := NewClient(server.Client())
	identity := worker.SandboxIdentity{Namespace: "ns", Name: "sandbox", UID: "uid-1"}
	ref := worker.SandboxSlotRef{SlotID: 0, Identity: identity}
	if err := client.ReserveSlot(ctx, server.URL, ref); err != nil {
		t.Fatalf("ReserveSlot() error = %v", err)
	}
	if err := client.StartSandbox(ctx, server.URL, worker.StartSandboxRequest{
		SlotID:    0,
		Identity:  identity,
		Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
	}); err != nil {
		t.Fatalf("StartSandbox() error = %v", err)
	}

	result, err := client.ExecSandbox(ctx, server.URL, worker.ExecSandboxRequest{
		SlotID:   0,
		Identity: identity,
		Command:  []string{"echo", "hello"},
	})
	if err != nil {
		t.Fatalf("ExecSandbox() error = %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "hello") {
		t.Fatalf("ExecSandbox() = %#v, want exit 0 stdout containing hello", result)
	}
}

type emptyRuntime struct {
	execStdout string
	created    runtime.ID
}

func (r *emptyRuntime) Create(_ context.Context, _ runtime.CreateRequest) (runtime.ID, error) {
	r.created = runtime.ID{Value: "runtime-1"}
	return r.created, nil
}
func (r *emptyRuntime) Start(context.Context, runtime.ID) error  { return nil }
func (r *emptyRuntime) Stop(context.Context, runtime.ID) error   { return nil }
func (r *emptyRuntime) Delete(context.Context, runtime.ID) error { return nil }
func (r *emptyRuntime) Status(context.Context, runtime.ID) (runtime.Status, error) {
	return runtime.Status{State: runtime.StateRunning}, nil
}
func (*emptyRuntime) List(context.Context) ([]runtime.Info, error) { return nil, nil }
func (r *emptyRuntime) Exec(_ context.Context, _ runtime.ID, req runtime.ExecRequest) (runtime.ExecResult, error) {
	stdout := r.execStdout
	if stdout == "" && len(req.Command) > 0 {
		stdout = req.Command[0]
	}
	return runtime.ExecResult{ExitCode: 0, Stdout: stdout}, nil
}
