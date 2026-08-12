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
		"proc":               true,
		"proc/1":             true,
		"sys/fs":             true,
		"dev/null":           true,
		"cid/rootfs/proc":    true,
		"cid/rootfs/proc/1":  true,
		"cid/rootfs/sys":     true,
		"cid/rootfs/dev/shm": true,
		"cid/rootfs/usr/bin": false,
		"usr/lib/proc":       false,
		"bin/app":            false,
	}
	for rel, want := range cases {
		if got := skipVirtualFSPath(rel); got != want {
			t.Fatalf("skipVirtualFSPath(%q)=%v, want %v", rel, got, want)
		}
	}
}

func TestArchiveVirtiofsSharesPacksFullShare(t *testing.T) {
	t.Parallel()
	share := t.TempDir()
	pauseRoot := filepath.Join(share, "pause", "rootfs")
	appRoot := filepath.Join(share, "appcid", "rootfs")
	for _, d := range []string{
		filepath.Join(pauseRoot, "usr", "bin"),
		filepath.Join(appRoot, "usr", "local", "bin"),
		filepath.Join(appRoot, "app"),
		filepath.Join(appRoot, "proc", "1"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pauseRoot, "usr", "bin", "pause"), []byte("pause"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appRoot, "usr", "local", "bin", "python"), []byte("py"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appRoot, "app", "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appRoot, "proc", "1", "status"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}

	shares := []virtiofsShare{{Tag: "kataShared", SharedDir: share, UpperTar: "stale-upper.tar"}}
	dest := t.TempDir()
	if err := archiveVirtiofsShares(shares, dest); err != nil {
		t.Fatalf("archiveVirtiofsShares: %v", err)
	}
	if shares[0].RootfsTar != "rootfs-share-0.tar" {
		t.Fatalf("RootfsTar = %q", shares[0].RootfsTar)
	}
	if shares[0].UpperTar != "" {
		t.Fatalf("UpperTar should be cleared, got %q", shares[0].UpperTar)
	}

	out := t.TempDir()
	if err := unpackRootfsTar(filepath.Join(dest, shares[0].RootfsTar), out); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	for _, rel := range []string{
		"pause/rootfs/usr/bin/pause",
		"appcid/rootfs/usr/local/bin/python",
		"appcid/rootfs/app/note.txt",
	} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s missing: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "appcid", "rootfs", "proc")); !os.IsNotExist(err) {
		t.Fatalf("rootfs/proc should be skipped, err=%v", err)
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
