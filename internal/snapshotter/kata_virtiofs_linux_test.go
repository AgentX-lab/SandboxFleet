//go:build linux

package snapshotter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReconstructShareFromImageRequiresIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := reconstructShareFromImage(ctx, dir, "", "python:3.12-slim"); err == nil {
		t.Fatal("expected error for empty container id")
	}
	if _, err := reconstructShareFromImage(ctx, dir, "cid", ""); err == nil {
		t.Fatal("expected error for empty image ref")
	}
}

func TestRemountBindReadOnly(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	dst := t.TempDir()
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("bind mount not permitted: %v", err)
		}
		t.Fatalf("bind mount: %v", err)
	}
	defer func() { _ = unix.Unmount(dst, unix.MNT_DETACH) }()

	if err := remountBindReadOnly(dst); err != nil {
		t.Fatalf("remountBindReadOnly: %v", err)
	}
	// Writing through the remounted bind should fail (EROFS).
	if err := os.WriteFile(filepath.Join(dst, "x"), []byte("no"), 0o644); err == nil {
		t.Fatal("expected EROFS after remountBindReadOnly")
	}
}
