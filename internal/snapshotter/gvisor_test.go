package snapshotter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGVisorSaveUsesRestoreRootForRestoredID(t *testing.T) {
	t.Parallel()
	g := &GVisor{
		RunscPath:   "runsc",
		SourceRoot:  "/run/containerd/runsc/k8s.io",
		RestoreRoot: "/var/lib/sandboxfleet/runsc",
	}
	name := "child-abcd1234"
	id := gvisorIDPrefix + name
	root, sandboxName := g.resolveCheckpointPaths(id)
	if sandboxName != name {
		t.Fatalf("sandboxName = %q, want %q", sandboxName, name)
	}
	wantRoot := filepath.Join(g.RestoreRoot, name)
	if root != wantRoot {
		t.Fatalf("root = %q, want %q", root, wantRoot)
	}
}

func TestGVisorSaveUsesSourceRootForCRIID(t *testing.T) {
	t.Parallel()
	g := &GVisor{
		SourceRoot:  "/run/containerd/runsc/k8s.io",
		RestoreRoot: "/var/lib/sandboxfleet/runsc",
	}
	id := "cri-sandbox-xyz"
	root, sandboxName := g.resolveCheckpointPaths(id)
	if sandboxName != id || root != g.SourceRoot {
		t.Fatalf("got root=%q name=%q", root, sandboxName)
	}
}

func TestGVisorCheckpointArgsPutIDLast(t *testing.T) {
	t.Parallel()
	args := gvisorCheckpointArgs("/run/root", "/tmp/img", "sandbox-1")
	if len(args) == 0 || args[len(args)-1] != "sandbox-1" {
		t.Fatalf("container id must be last: %#v", args)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "checkpoint --image-path /tmp/img --leave-running sandbox-1") {
		t.Fatalf("unexpected checkpoint argv: %#v", args)
	}
}

func TestGVisorCreateAndRestoreArgs(t *testing.T) {
	t.Parallel()
	create := gvisorCreateArgs("/var/runsc/child", "/var/runsc/child/bundle", "child-1")
	if len(create) == 0 || create[len(create)-1] != "child-1" {
		t.Fatalf("create id must be last: %#v", create)
	}
	if strings.Join(create, " ") != "--root /var/runsc/child --network=host create --bundle /var/runsc/child/bundle child-1" {
		t.Fatalf("unexpected create argv: %#v", create)
	}

	restore := gvisorRestoreArgs("/var/runsc/child", "/var/runsc/child/bundle", "/tmp/img", "child-1")
	if len(restore) == 0 || restore[len(restore)-1] != "child-1" {
		t.Fatalf("restore id must be last: %#v", restore)
	}
	joined := strings.Join(restore, " ")
	if !strings.Contains(joined, "restore --bundle /var/runsc/child/bundle --image-path /tmp/img --direct --background --detach child-1") {
		t.Fatalf("unexpected restore argv: %#v", restore)
	}
}

func TestWriteGVisorRestoreBundle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle")
	name := "child-abcd"
	if err := writeGVisorRestoreBundle(bundle, name); err != nil {
		t.Fatalf("writeGVisorRestoreBundle: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatalf("config.json missing: %v", err)
	}
	got := restoreCgroupsPath(name)
	if !strings.Contains(string(raw), got) {
		t.Fatalf("config missing cgroupsPath %q: %s", got, raw)
	}
	if strings.Contains(got, ":") {
		t.Fatalf("cgroupsPath must be colon-free for cgroupfs, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(bundle, "rootfs")); err != nil {
		t.Fatalf("rootfs missing: %v", err)
	}
}
