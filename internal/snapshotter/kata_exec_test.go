package snapshotter

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestKataExecProcessDefaultPATH(t *testing.T) {
	t.Parallel()
	got := kataExecProcess([]string{"python", "-c", "print(1)"})
	if got == nil {
		t.Fatal("nil process")
	}
	if got.Terminal {
		t.Fatal("Terminal want false")
	}
	if got.Cwd != "/" {
		t.Fatalf("Cwd = %q, want /", got.Cwd)
	}
	if !slices.Equal(got.Args, []string{"python", "-c", "print(1)"}) {
		t.Fatalf("Args = %#v", got.Args)
	}
	if !slices.Contains(got.Env, kataDefaultPATH) {
		t.Fatalf("Env missing %q: %#v", kataDefaultPATH, got.Env)
	}
}

func TestResolveKataArgv0(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	usr := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(usr, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usr, "python"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolveKataArgv0(root, []string{"sleep", "3600"})
	if !slices.Equal(got, []string{"/bin/sleep", "3600"}) {
		t.Fatalf("sleep = %#v", got)
	}
	got = resolveKataArgv0(root, []string{"python", "-c", "print(1)"})
	if !slices.Equal(got, []string{"/usr/local/bin/python", "-c", "print(1)"}) {
		t.Fatalf("python = %#v", got)
	}
	got = resolveKataArgv0(root, []string{"/bin/sleep", "1"})
	if !slices.Equal(got, []string{"/bin/sleep", "1"}) {
		t.Fatalf("abs = %#v", got)
	}
	got = resolveKataArgv0(root, []string{"missing"})
	if !slices.Equal(got, []string{"missing"}) {
		t.Fatalf("missing = %#v", got)
	}
	if resolveKataArgv0("", []string{"sleep"})[0] != "sleep" {
		t.Fatal("empty rootfs should skip")
	}
}
