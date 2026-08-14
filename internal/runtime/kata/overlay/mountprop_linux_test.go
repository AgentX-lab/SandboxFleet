//go:build linux

package overlay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureSharedPropagationIdempotent(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to create bind mounts")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "kata-containers")
	if err := EnsureSharedPropagation(path); err != nil {
		t.Fatalf("first EnsureSharedPropagation: %v", err)
	}
	if err := EnsureSharedPropagation(path); err != nil {
		t.Fatalf("second EnsureSharedPropagation: %v", err)
	}
	if !alreadySharedMount(path) {
		t.Fatalf("%s not reported as shared after EnsureSharedPropagation", path)
	}
}
