package snapshotter

import (
	"encoding/json"
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
	// Checkpoint the pause/root container, matching substrate.
	if sandboxName != "pause" {
		t.Fatalf("sandboxName = %q, want pause", sandboxName)
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
	create := gvisorCreateArgs("/var/runsc/child", "/var/runsc/child/bundle", "child-1", "")
	if len(create) == 0 || create[len(create)-1] != "child-1" {
		t.Fatalf("create id must be last: %#v", create)
	}
	joinedCreate := strings.Join(create, " ")
	if !strings.Contains(joinedCreate, "--restore-spec-validation=ignore create --bundle /var/runsc/child/bundle child-1") {
		t.Fatalf("unexpected create argv: %#v", create)
	}

	restore := gvisorRestoreArgs("/var/runsc/child", "/var/runsc/child/bundle", "/tmp/img", "child-1", "")
	if len(restore) == 0 || restore[len(restore)-1] != "child-1" {
		t.Fatalf("restore id must be last: %#v", restore)
	}
	joined := strings.Join(restore, " ")
	if !strings.Contains(joined, "--restore-spec-validation=ignore restore --bundle /var/runsc/child/bundle --image-path /tmp/img --background --direct --detach child-1") {
		t.Fatalf("unexpected restore argv: %#v", restore)
	}

	withDebug := gvisorRestoreArgs("/var/runsc/child", "/var/runsc/child/bundle", "/tmp/img", "child-1", "/tmp/debug")
	if !strings.Contains(strings.Join(withDebug, " "), "--debug --debug-log /tmp/debug/ --alsologtostderr") {
		t.Fatalf("expected debug flags: %#v", withDebug)
	}
}

func TestWriteGVisorRestoreBundle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle")
	c := gvisorRestoreContainer{ID: "app", Name: "snap-parent"}
	if err := writeGVisorRestoreBundle(bundle, c, "pause"); err != nil {
		t.Fatalf("writeGVisorRestoreBundle: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatalf("config.json missing: %v", err)
	}
	got := restoreCgroupsPath("app")
	if !strings.Contains(string(raw), got) {
		t.Fatalf("config missing cgroupsPath %q: %s", got, raw)
	}
	if strings.Contains(got, ":") {
		t.Fatalf("cgroupsPath must be colon-free for cgroupfs, got %q", got)
	}
	if !strings.Contains(string(raw), `"ociVersion": "1.1.0"`) {
		t.Fatalf("want ociVersion 1.1.0: %s", raw)
	}
	if !strings.Contains(string(raw), `"io.kubernetes.cri.container-type": "container"`) {
		t.Fatalf("missing container-type: %s", raw)
	}
	if !strings.Contains(string(raw), `"io.kubernetes.cri.sandbox-id": "pause"`) {
		t.Fatalf("missing sandbox-id: %s", raw)
	}
	if !strings.Contains(string(raw), `"io.kubernetes.cri.container-name": "snap-parent"`) {
		t.Fatalf("missing container-name: %s", raw)
	}
	for _, dst := range []string{"/etc/hosts", "/etc/hostname", "/etc/resolv.conf"} {
		if !strings.Contains(string(raw), `"`+dst+`"`) {
			t.Fatalf("missing mount %q: %s", dst, raw)
		}
	}
	for _, name := range []string{"hosts", "hostname", "resolv.conf"} {
		if _, err := os.Stat(filepath.Join(bundle, "etc", name)); err != nil {
			t.Fatalf("etc/%s missing: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(bundle, "rootfs")); err != nil {
		t.Fatalf("rootfs missing: %v", err)
	}
}

func TestWriteGVisorRestoreBundleSandbox(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle")
	c := gvisorRestoreContainer{ID: "pause", Sandbox: true}
	if err := writeGVisorRestoreBundle(bundle, c, "pause"); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), `"io.kubernetes.cri.container-type": "sandbox"`) {
		t.Fatalf("want sandbox type: %s", raw)
	}
	if strings.Contains(string(raw), annotationContainerName) {
		t.Fatalf("pause must not set container-name: %s", raw)
	}
	if strings.Contains(string(raw), `"mounts"`) {
		t.Fatalf("pause must not add etc mounts: %s", raw)
	}
}

func TestGVisorContainersFileRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := gvisorCRIContainers("snap-parent")
	if err := writeGVisorContainersFile(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readGVisorContainersFile(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 || got[0].ID != "pause" || !got[0].Sandbox || got[1].Name != "snap-parent" {
		t.Fatalf("got %#v", got)
	}
	if gvisorAppContainerID(got) != "app" {
		t.Fatalf("app id = %q", gvisorAppContainerID(got))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, gvisorContainersFile))
	var decoded []gvisorRestoreContainer
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json: %v", err)
	}
}
