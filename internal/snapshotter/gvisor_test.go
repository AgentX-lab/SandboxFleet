package snapshotter

import (
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
