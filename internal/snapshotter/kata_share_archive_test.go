package snapshotter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackAndUnpackRootfsTar(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "app"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Virtual FS dirs should be skipped.
	if err := os.MkdirAll(filepath.Join(src, "proc", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "proc", "1", "status"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(t.TempDir(), "share.tar")
	if err := packRootfsTar(src, tarPath); err != nil {
		t.Fatalf("archive: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := unpackRootfsTar(tarPath, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "bin", "app"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("bin/app = %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "proc")); !os.IsNotExist(err) {
		t.Fatalf("proc should be skipped, err=%v", err)
	}
}

func TestRootfsTarFileName(t *testing.T) {
	t.Parallel()
	if got := rootfsTarFileName(0); got != "rootfs-share-0.tar" {
		t.Fatalf("got %q", got)
	}
}
