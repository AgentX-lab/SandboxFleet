package snapshotter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindVMUsesOnlyAPISocket(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sandboxID := "sbx-test"
	vmDir := filepath.Join(root, "vm", sandboxID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Both sockets present: vsock must not win.
	if err := os.WriteFile(filepath.Join(vmDir, "clh.sock"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	api := filepath.Join(vmDir, "clh-api.sock")
	if err := os.WriteFile(api, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	k := &Kata{SocketSearchRoots: []string{filepath.Join(root, "vm")}}
	gotDir, gotSock, err := k.findVM(sandboxID)
	if err != nil {
		t.Fatalf("findVM: %v", err)
	}
	if gotDir != vmDir {
		t.Fatalf("vmDir = %q, want %q", gotDir, vmDir)
	}
	if gotSock != api {
		t.Fatalf("apiSocket = %q, want %q", gotSock, api)
	}
}

func TestFindVMIgnoresVsockOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sandboxID := "sbx-vsock-only"
	vmDir := filepath.Join(root, "vm", sandboxID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "clh.sock"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	k := &Kata{SocketSearchRoots: []string{filepath.Join(root, "vm")}}
	if _, _, err := k.findVM(sandboxID); err == nil {
		t.Fatal("expected error when only clh.sock exists")
	}
}
