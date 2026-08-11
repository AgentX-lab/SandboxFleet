package snapshotter

import (
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
