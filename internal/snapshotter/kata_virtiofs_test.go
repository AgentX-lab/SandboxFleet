package snapshotter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteRestoreSocketsByTag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vmDir := filepath.Join(dir, "vm")
	snap := filepath.Join(dir, "snap")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"vsock":  map[string]any{"socket": "/old/clh.sock"},
		"serial": map[string]any{"mode": "File", "file": "/old/serial.log"},
		"fs": []any{
			map[string]any{"tag": "durable", "socket": "/old/virtiofsd-durable.sock"},
			map[string]any{"tag": "kataShared", "socket": "/old/virtiofsd.sock"},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// Meta order deliberately differs from config fs order; tag must win.
	meta := []virtiofsShare{
		{Tag: "kataShared", SharedDir: "/shared/ro"},
		{Tag: "durable", SharedDir: "/shared/durable"},
	}
	planned, err := rewriteRestoreSockets(snap, vmDir, meta)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if len(planned) != 2 {
		t.Fatalf("planned len = %d, want 2", len(planned))
	}
	if planned[0].SharedDir != "/shared/durable" || planned[0].Tag != "durable" {
		t.Fatalf("planned[0] = %+v, want durable share", planned[0])
	}
	if planned[1].SharedDir != "/shared/ro" || planned[1].Tag != "kataShared" {
		t.Fatalf("planned[1] = %+v, want kataShared share", planned[1])
	}
	if planned[0].Socket != filepath.Join(vmDir, "virtiofsd-0.sock") {
		t.Fatalf("socket0 = %q", planned[0].Socket)
	}

	out, err := os.ReadFile(filepath.Join(snap, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	vsock := got["vsock"].(map[string]any)
	if vsock["socket"] != filepath.Join(vmDir, "clh.sock") {
		t.Fatalf("vsock = %v", vsock["socket"])
	}
	fss := got["fs"].([]any)
	fm0 := fss[0].(map[string]any)
	if fm0["socket"] != filepath.Join(vmDir, "virtiofsd-0.sock") {
		t.Fatalf("fs0 socket = %v", fm0["socket"])
	}
}

func TestRewriteRestoreSocketsMissingShare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snap := filepath.Join(dir, "snap")
	vmDir := filepath.Join(dir, "vm")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"fs": []any{
			map[string]any{"tag": "kataShared", "socket": "/old.sock"},
		},
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(snap, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Socket rewrite no longer requires SharedDir; materialize resolves it later.
	planned, err := rewriteRestoreSockets(snap, vmDir, nil)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if len(planned) != 1 || planned[0].SharedDir != "" {
		t.Fatalf("planned = %+v", planned)
	}
	if _, err := findLiveParentRootfs(planned[0], nil); err == nil {
		t.Fatal("expected findLiveParentRootfs error when nothing live")
	}
}

func TestFindLiveParentRootfsPrefersMetaThenLive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	liveDir := filepath.Join(root, "live")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findLiveParentRootfs(
		virtiofsShare{Tag: "kataShared", SharedDir: metaDir},
		[]virtiofsShare{{Tag: "kataShared", SharedDir: liveDir}},
	)
	if err != nil || got != metaDir {
		t.Fatalf("prefer meta: got %q err=%v", got, err)
	}

	stale := filepath.Join(root, "gone")
	got, err = findLiveParentRootfs(
		virtiofsShare{Tag: "kataShared", SharedDir: stale},
		[]virtiofsShare{{Tag: "kataShared", SharedDir: liveDir}},
	)
	if err != nil || got != liveDir {
		t.Fatalf("fallback live: got %q err=%v", got, err)
	}
}

func TestChildRootfsDir(t *testing.T) {
	t.Parallel()
	got := childRootfsDir("/var/lib/sandboxfleet/kata/child", 0)
	want := "/var/lib/sandboxfleet/kata/child/virtiofs/0"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDiscoverRootfsRelPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "rootfs", "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "b", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := discoverRootfsRelPaths(root)
	if len(got) != 2 || got[0] != "a/rootfs" || got[1] != "b/rootfs" {
		t.Fatalf("got %#v", got)
	}
}

func TestDedupeVirtiofsShares(t *testing.T) {
	t.Parallel()
	in := []virtiofsShare{
		{Tag: "kataShared", SharedDir: "/run/share"},
		{Tag: "kataShared", SharedDir: "/run/share"},
		{Tag: "other", SharedDir: "/run/other"},
	}
	got := dedupeVirtiofsShares(in)
	if len(got) != 2 {
		t.Fatalf("got %d shares: %+v", len(got), got)
	}
	if got[0].SharedDir != "/run/share" || got[1].SharedDir != "/run/other" {
		t.Fatalf("got %+v", got)
	}
}
