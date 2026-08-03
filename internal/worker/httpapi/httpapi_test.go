package httpapi

import (
	"context"
	"net/http/httptest"
	"path"
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

func TestClientAndServerFileOps(t *testing.T) {
	ctx := context.Background()
	manager := worker.NewSlotManager(worker.Config{Slots: 1}, &fileRuntime{files: map[string][]byte{}})
	server := httptest.NewServer(NewServer(manager))
	defer server.Close()

	client := NewClient(server.Client())
	identity := worker.SandboxIdentity{Namespace: "ns", Name: "sandbox", UID: "uid-1"}
	if err := client.ReserveSlot(ctx, server.URL, worker.SandboxSlotRef{SlotID: 0, Identity: identity}); err != nil {
		t.Fatalf("ReserveSlot() error = %v", err)
	}
	if err := client.StartSandbox(ctx, server.URL, worker.StartSandboxRequest{
		SlotID:    0,
		Identity:  identity,
		Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
	}); err != nil {
		t.Fatalf("StartSandbox() error = %v", err)
	}

	req := worker.SandboxFileRequest{SlotID: 0, Identity: identity, Path: "hello.txt"}
	content := []byte("file-bytes")
	if err := client.WriteSandboxFile(ctx, server.URL, req, content); err != nil {
		t.Fatalf("WriteSandboxFile() error = %v", err)
	}
	exists, err := client.ExistsSandboxFile(ctx, server.URL, req)
	if err != nil {
		t.Fatalf("ExistsSandboxFile() error = %v", err)
	}
	if !exists {
		t.Fatal("ExistsSandboxFile() = false, want true")
	}
	got, err := client.ReadSandboxFile(ctx, server.URL, req)
	if err != nil {
		t.Fatalf("ReadSandboxFile() error = %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("ReadSandboxFile() = %q, want %q", got, content)
	}
	entries, err := client.ListSandboxFiles(ctx, server.URL, worker.SandboxFileRequest{SlotID: 0, Identity: identity, Path: "."})
	if err != nil {
		t.Fatalf("ListSandboxFiles() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "hello.txt" || entries[0].Type != worker.FileTypeFile {
		t.Fatalf("ListSandboxFiles() = %#v", entries)
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
func (*emptyRuntime) ReadFile(context.Context, runtime.ID, string) ([]byte, error) {
	return nil, runtime.ErrNotFound
}
func (*emptyRuntime) WriteFile(context.Context, runtime.ID, string, []byte) error {
	return nil
}
func (*emptyRuntime) ListFiles(context.Context, runtime.ID, string) ([]runtime.FileEntry, error) {
	return nil, nil
}
func (*emptyRuntime) FileExists(context.Context, runtime.ID, string) (bool, error) {
	return false, nil
}

type fileRuntime struct {
	id    runtime.ID
	files map[string][]byte
}

func (r *fileRuntime) Create(context.Context, runtime.CreateRequest) (runtime.ID, error) {
	r.id = runtime.ID{Value: "runtime-1"}
	return r.id, nil
}
func (*fileRuntime) Start(context.Context, runtime.ID) error  { return nil }
func (*fileRuntime) Stop(context.Context, runtime.ID) error   { return nil }
func (*fileRuntime) Delete(context.Context, runtime.ID) error { return nil }
func (*fileRuntime) Status(context.Context, runtime.ID) (runtime.Status, error) {
	return runtime.Status{State: runtime.StateRunning}, nil
}
func (*fileRuntime) List(context.Context) ([]runtime.Info, error) { return nil, nil }
func (*fileRuntime) Exec(context.Context, runtime.ID, runtime.ExecRequest) (runtime.ExecResult, error) {
	return runtime.ExecResult{ExitCode: 0}, nil
}

func (r *fileRuntime) ReadFile(_ context.Context, _ runtime.ID, absPath string) ([]byte, error) {
	data, ok := r.files[absPath]
	if !ok {
		return nil, runtime.ErrNotFound
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

func (r *fileRuntime) WriteFile(_ context.Context, _ runtime.ID, absPath string, content []byte) error {
	data := make([]byte, len(content))
	copy(data, content)
	r.files[absPath] = data
	return nil
}

func (r *fileRuntime) ListFiles(_ context.Context, _ runtime.ID, absPath string) ([]runtime.FileEntry, error) {
	entries := make([]runtime.FileEntry, 0)
	for name := range r.files {
		if path.Dir(name) != absPath {
			continue
		}
		entries = append(entries, runtime.FileEntry{Name: path.Base(name), Type: worker.FileTypeFile})
	}
	return entries, nil
}

func (r *fileRuntime) FileExists(_ context.Context, _ runtime.ID, absPath string) (bool, error) {
	if absPath == worker.DefaultFilesRoot {
		return true, nil
	}
	_, ok := r.files[absPath]
	return ok, nil
}
