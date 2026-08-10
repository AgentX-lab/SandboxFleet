//go:build linux

package snapshotter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlaceSnapshotDirRenamesWhenPossible(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "memory-ranges"), []byte("snap"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := placeSnapshotDir(src, dst); err != nil {
		t.Fatalf("placeSnapshotDir: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src still exists after rename: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "memory-ranges"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "snap" {
		t.Fatalf("content = %q, want snap", got)
	}
}
