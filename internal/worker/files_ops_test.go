package worker

import (
	"context"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/runtime"
)

func TestFileWriteReadListExists(t *testing.T) {
	ctx := context.Background()
	rt := &memFSRuntime{files: map[string][]byte{}}
	manager := NewSlotManager(Config{Slots: 1}, rt)
	identity := SandboxIdentity{Namespace: "ns", Name: "sandbox", UID: "uid-1"}
	if err := manager.ReserveSlot(ctx, SandboxSlotRef{SlotID: 0, Identity: identity}); err != nil {
		t.Fatalf("ReserveSlot() error = %v", err)
	}
	if err := manager.StartSandbox(ctx, StartSandboxRequest{
		SlotID:    0,
		Identity:  identity,
		Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
	}); err != nil {
		t.Fatalf("StartSandbox() error = %v", err)
	}

	content := []byte("hello fleet")
	if err := manager.WriteSandboxFile(ctx, SandboxFileRequest{SlotID: 0, Identity: identity, Path: "note.txt"}, content); err != nil {
		t.Fatalf("WriteSandboxFile() error = %v", err)
	}
	exists, err := manager.ExistsSandboxFile(ctx, SandboxFileRequest{SlotID: 0, Identity: identity, Path: "note.txt"})
	if err != nil {
		t.Fatalf("ExistsSandboxFile() error = %v", err)
	}
	if !exists {
		t.Fatal("ExistsSandboxFile() = false, want true")
	}
	got, err := manager.ReadSandboxFile(ctx, SandboxFileRequest{SlotID: 0, Identity: identity, Path: "note.txt"})
	if err != nil {
		t.Fatalf("ReadSandboxFile() error = %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("ReadSandboxFile() = %q, want %q", got, content)
	}
	entries, err := manager.ListSandboxFiles(ctx, SandboxFileRequest{SlotID: 0, Identity: identity, Path: "."})
	if err != nil {
		t.Fatalf("ListSandboxFiles() error = %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name == "note.txt" && entry.Type == FileTypeFile {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListSandboxFiles() = %#v, want note.txt file entry", entries)
	}
}

type memFSRuntime struct {
	id    runtime.ID
	files map[string][]byte
}

func (r *memFSRuntime) Create(context.Context, runtime.CreateRequest) (runtime.ID, error) {
	r.id = runtime.ID{Value: "runtime-1"}
	return r.id, nil
}
func (*memFSRuntime) Start(context.Context, runtime.ID) error  { return nil }
func (*memFSRuntime) Stop(context.Context, runtime.ID) error   { return nil }
func (*memFSRuntime) Delete(context.Context, runtime.ID) error { return nil }
func (*memFSRuntime) Status(context.Context, runtime.ID) (runtime.Status, error) {
	return runtime.Status{State: runtime.StateRunning}, nil
}
func (*memFSRuntime) List(context.Context) ([]runtime.Info, error) { return nil, nil }
func (*memFSRuntime) Exec(context.Context, runtime.ID, runtime.ExecRequest) (runtime.ExecResult, error) {
	return runtime.ExecResult{ExitCode: 0}, nil
}

func (r *memFSRuntime) ReadFile(_ context.Context, _ runtime.ID, absPath string) ([]byte, error) {
	data, ok := r.files[absPath]
	if !ok {
		return nil, runtime.ErrNotFound
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

func (r *memFSRuntime) WriteFile(_ context.Context, _ runtime.ID, absPath string, content []byte) error {
	data := make([]byte, len(content))
	copy(data, content)
	r.files[absPath] = data
	return nil
}

func (r *memFSRuntime) ListFiles(_ context.Context, _ runtime.ID, absPath string) ([]runtime.FileEntry, error) {
	entries := make([]runtime.FileEntry, 0)
	for name := range r.files {
		if parentDir(name) == absPath {
			entries = append(entries, runtime.FileEntry{Name: baseName(name), Type: FileTypeFile})
		}
	}
	return entries, nil
}

func (r *memFSRuntime) FileExists(_ context.Context, _ runtime.ID, absPath string) (bool, error) {
	if absPath == DefaultFilesRoot {
		return true, nil
	}
	_, ok := r.files[absPath]
	return ok, nil
}

func parentDir(value string) string {
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] == '/' {
			if i == 0 {
				return "/"
			}
			return value[:i]
		}
	}
	return "."
}

func baseName(value string) string {
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] == '/' {
			return value[i+1:]
		}
	}
	return value
}
