package snapshotter

import (
	"context"
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
	create := gvisorCreateArgs("/var/runsc/child", "/var/runsc/child/bundle", "child-1", "", false)
	if len(create) == 0 || create[len(create)-1] != "child-1" {
		t.Fatalf("create id must be last: %#v", create)
	}
	joinedCreate := strings.Join(create, " ")
	if !strings.Contains(joinedCreate, "--restore-spec-validation=ignore create --bundle /var/runsc/child/bundle child-1") {
		t.Fatalf("unexpected create argv: %#v", create)
	}
	if strings.Contains(joinedCreate, "--overlay2=none") {
		t.Fatalf("app create must keep default overlay2: %#v", create)
	}

	createPause := gvisorCreateArgs("/var/runsc/child", "/var/runsc/child/bundle", "pause", "", true)
	joinedPause := strings.Join(createPause, " ")
	if !strings.Contains(joinedPause, "--overlay2=none") {
		t.Fatalf("pause create needs --overlay2=none: %#v", createPause)
	}

	restore := gvisorRestoreArgs("/var/runsc/child", "/var/runsc/child/bundle", "/tmp/img", "child-1", "", false)
	if len(restore) == 0 || restore[len(restore)-1] != "child-1" {
		t.Fatalf("restore id must be last: %#v", restore)
	}
	joined := strings.Join(restore, " ")
	if !strings.Contains(joined, "--restore-spec-validation=ignore restore --bundle /var/runsc/child/bundle --image-path /tmp/img --background --direct --detach child-1") {
		t.Fatalf("unexpected restore argv: %#v", restore)
	}
	if strings.Contains(joined, "--overlay2=none") {
		t.Fatalf("app restore must keep default overlay2: %#v", restore)
	}

	restorePause := gvisorRestoreArgs("/var/runsc/child", "/var/runsc/child/bundle", "/tmp/img", "pause", "", true)
	if !strings.Contains(strings.Join(restorePause, " "), "--overlay2=none") {
		t.Fatalf("pause restore needs --overlay2=none: %#v", restorePause)
	}

	withDebug := gvisorRestoreArgs("/var/runsc/child", "/var/runsc/child/bundle", "/tmp/img", "child-1", "/tmp/debug", false)
	if !strings.Contains(strings.Join(withDebug, " "), "--debug --debug-log /tmp/debug/ --alsologtostderr") {
		t.Fatalf("expected debug flags: %#v", withDebug)
	}
}

func TestWriteGVisorRestoreConfig(t *testing.T) {
	t.Parallel()
	bundle := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := gvisorRestoreContainer{ID: "snap-parent", Name: "snap-parent", Image: "python:3.12-slim"}
	if err := writeGVisorRestoreConfig(bundle, c, "pause"); err != nil {
		t.Fatalf("writeGVisorRestoreConfig: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatalf("config.json missing: %v", err)
	}
	got := restoreCgroupsPath("snap-parent")
	if !strings.Contains(string(raw), got) {
		t.Fatalf("config missing cgroupsPath %q: %s", got, raw)
	}
	if !strings.Contains(string(raw), `"ociVersion": "1.1.0"`) {
		t.Fatalf("want ociVersion 1.1.0: %s", raw)
	}
	if !strings.Contains(string(raw), `"io.kubernetes.cri.container-name": "snap-parent"`) {
		t.Fatalf("missing container-name: %s", raw)
	}
	for _, dst := range []string{"/etc/hosts", "/etc/hostname", "/etc/resolv.conf"} {
		if !strings.Contains(string(raw), `"`+dst+`"`) {
			t.Fatalf("missing mount %q: %s", dst, raw)
		}
	}
}

func TestWriteGVisorRestoreConfigSandbox(t *testing.T) {
	t.Parallel()
	bundle := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := gvisorRestoreContainer{ID: "pause", Sandbox: true, Image: pauseImageRef()}
	if err := writeGVisorRestoreConfig(bundle, c, "pause"); err != nil {
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
		t.Fatalf("CRI pause must omit container-name (__no_name_0): %s", raw)
	}
	if !strings.Contains(string(raw), `"/etc/resolv.conf"`) {
		t.Fatalf("pause must bind /etc/resolv.conf like substrate: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(bundle, "rootfs", "etc", "resolv.conf")); err != nil {
		t.Fatalf("rootfs etc/resolv.conf missing: %v", err)
	}
}

func TestWriteGVisorRestoreBundleRequiresImage(t *testing.T) {
	t.Parallel()
	err := writeGVisorRestoreBundle(context.Background(), t.TempDir(), gvisorRestoreContainer{ID: "snap-parent"}, "pause")
	if err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestFillGVisorContainerImages(t *testing.T) {
	t.Parallel()
	containers := []gvisorRestoreContainer{
		{ID: "pause", Sandbox: true},
		{ID: "snap-parent", Name: "snap-parent"},
	}
	if err := fillGVisorContainerImages(containers, "python:3.12-slim"); err != nil {
		t.Fatal(err)
	}
	if containers[0].Image != pauseImageRef() {
		t.Fatalf("pause image = %q", containers[0].Image)
	}
	if containers[1].Image != "python:3.12-slim" {
		t.Fatalf("app image = %q", containers[1].Image)
	}
}

func TestGVisorContainersFileRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := gvisorCRIContainers("snap-parent", "python:3.12-slim")
	if err := writeGVisorContainersFile(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readGVisorContainersFile(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 || got[0].ID != "pause" || got[0].Name != "" || !got[0].Sandbox || got[1].ID != "snap-parent" || got[1].Name != "snap-parent" {
		t.Fatalf("got %#v", got)
	}
	if got[1].Image != "python:3.12-slim" {
		t.Fatalf("image not round-tripped: %#v", got)
	}
	if gvisorAppContainerID(got) != "snap-parent" {
		t.Fatalf("app id = %q", gvisorAppContainerID(got))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, gvisorContainersFile))
	var decoded []gvisorRestoreContainer
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json: %v", err)
	}
}
