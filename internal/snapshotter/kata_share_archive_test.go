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
	// Virtual FS dirs should be skipped at share root and under container rootfs.
	if err := os.MkdirAll(filepath.Join(src, "proc", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "proc", "1", "status"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "cid", "rootfs", "proc", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "cid", "rootfs", "proc", "1", "status"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "cid", "rootfs", "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "cid", "rootfs", "usr", "bin", "python"), []byte("#!"), 0o755); err != nil {
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
	if _, err := os.Stat(filepath.Join(dst, "cid", "rootfs", "proc")); !os.IsNotExist(err) {
		t.Fatalf("cid/rootfs/proc should be skipped, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "cid", "rootfs", "usr", "bin", "python")); err != nil {
		t.Fatalf("python missing after unpack: %v", err)
	}
}

func TestSkipVirtualFSPath(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"proc":                 true,
		"proc/1":               true,
		"sys/fs":               true,
		"dev/null":             true,
		"cid/rootfs/proc":      true,
		"cid/rootfs/proc/1":    true,
		"cid/rootfs/sys":       true,
		"cid/rootfs/dev/shm":   true,
		"cid/rootfs/usr/bin":   false,
		"usr/lib/proc":         false,
		"bin/app":              false,
	}
	for rel, want := range cases {
		if got := skipVirtualFSPath(rel); got != want {
			t.Fatalf("skipVirtualFSPath(%q)=%v, want %v", rel, got, want)
		}
	}
}

func TestRootfsUpperTarFileName(t *testing.T) {
	t.Parallel()
	if got := rootfsUpperTarFileName(); got != "rootfs-upper.tar" {
		t.Fatalf("got %q", got)
	}
}

func TestMergeUpperFromTar(t *testing.T) {
	t.Parallel()
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "usr", "bin", "python"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	upperSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(upperSrc, "snap-note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "upper.tar")
	if err := packRootfsTar(upperSrc, tarPath); err != nil {
		t.Fatal(err)
	}
	if err := mergeUpperFromTar(tarPath, rootfs); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rootfs, "snap-note.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("snap-note.txt = %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "usr", "bin", "python")); err != nil {
		t.Fatalf("python should remain: %v", err)
	}
}

func TestIsKataSharedTag(t *testing.T) {
	t.Parallel()
	if !isKataSharedTag("kataShared") || !isKataSharedTag("") {
		t.Fatal("want kataShared tags")
	}
	if isKataSharedTag("durable") {
		t.Fatal("durable is not kataShared")
	}
}

