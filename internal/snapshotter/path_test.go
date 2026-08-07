package snapshotter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirstExistingPathPrefersExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	libexec := filepath.Join(dir, "libexec", "virtiofsd")
	bin := filepath.Join(dir, "bin", "virtiofsd")
	if err := os.MkdirAll(filepath.Dir(libexec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libexec, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := firstExistingPath(bin, libexec, "virtiofsd")
	if got != libexec {
		t.Fatalf("got %q, want %q", got, libexec)
	}
}

func TestFirstExistingPathFallsBackToBareName(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	got := firstExistingPath(missing, "virtiofsd")
	if got != "virtiofsd" {
		t.Fatalf("got %q, want virtiofsd", got)
	}
}
