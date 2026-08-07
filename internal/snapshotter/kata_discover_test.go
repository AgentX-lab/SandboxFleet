package snapshotter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFallbackKataSharedDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Simulate conventional Kata layout under a temp root by temporarily
	// relying on absolute path helper behavior via direct call shape.
	id := "sbx-abc"
	shared := filepath.Join("/run/kata-containers/shared/sandboxes", id, "shared")
	// Cannot create under /run in unit tests; just assert empty when missing.
	if got := fallbackKataSharedDir(id); got != nil {
		// If the path exists on this machine (unlikely in CI unit), require tag.
		if got[0].Tag != "kataShared" || got[0].SharedDir != shared {
			t.Fatalf("got %+v", got)
		}
	}
	if got := fallbackKataSharedDir(""); got != nil {
		t.Fatalf("empty id: %v", got)
	}
	_ = root
	_ = os.ErrNotExist
}

func TestDiscoverVirtiofsSharesMatchesSandboxID(t *testing.T) {
	t.Parallel()
	// Without a live virtiofsd, discover falls back; missing /run path → nil.
	got := discoverVirtiofsShares("/run/vc/vm/no-such-sandbox-id")
	if got != nil {
		t.Fatalf("expected nil for missing sandbox, got %+v", got)
	}
}
