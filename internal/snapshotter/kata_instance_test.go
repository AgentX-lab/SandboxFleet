package snapshotter

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

func TestKataInstanceRoundTrip(t *testing.T) {
	t.Parallel()
	k := &Kata{StateDir: t.TempDir()}
	inst := kataInstance{
		Name:        "sb-1234abcd",
		Namespace:   "ns",
		SandboxName: "sb",
		UID:         "1234abcd-uid",
		SlotID:      2,
		ContainerID: "sb" + kataOverlaySuffix,
		BaseID:      "1234abcd-uid",
		PID:         4242,
	}
	if err := k.saveInstance(inst); err != nil {
		t.Fatalf("saveInstance: %v", err)
	}

	got, err := k.Instance(sandboxruntime.ID{Value: kataIDPrefix + inst.Name})
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	if got.ContainerID != inst.ContainerID || got.SlotID != 2 || got.PID != 4242 {
		t.Fatalf("Instance = %+v", got)
	}
	if got.Identity.Namespace != "ns" || got.Identity.Name != "sb" || string(got.Identity.UID) != inst.UID {
		t.Fatalf("Instance identity = %+v", got.Identity)
	}

	all, err := k.Instances()
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(all) != 1 || all[0].ID.Value != kataIDPrefix+inst.Name {
		t.Fatalf("Instances = %+v", all)
	}
}

func TestKataInstanceMissing(t *testing.T) {
	t.Parallel()
	k := &Kata{StateDir: t.TempDir()}
	if _, err := k.Instance(sandboxruntime.ID{Value: "runsc:other"}); err == nil {
		t.Fatal("Instance(runsc id) = nil error, want error")
	}
	// A gone instance must be ErrNotFound so the Worker cleans the slot up
	// instead of treating it as a transient failure.
	_, err := k.Instance(sandboxruntime.ID{Value: kataIDPrefix + "absent"})
	if !errors.Is(err, sandboxruntime.ErrNotFound) {
		t.Fatalf("Instance(absent) error = %v, want ErrNotFound", err)
	}
	all, err := k.Instances()
	if err != nil || len(all) != 0 {
		t.Fatalf("Instances on empty state dir = %v, %v", all, err)
	}
}

func TestReadKataBaseIDFileWins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	meta := kataMeta{BaseID: "from-meta"}
	if got := readKataBaseID(dir, meta); got != "from-meta" {
		t.Fatalf("readKataBaseID without file = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, kataBaseIDFile), []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readKataBaseID(dir, meta); got != "from-file" {
		t.Fatalf("readKataBaseID with file = %q", got)
	}
}

func TestKataCarrierID(t *testing.T) {
	t.Parallel()
	if got := kataCarrierID(kataMeta{AppContainerName: "app", ContainerID: "other_ovl"}); got != "app" {
		t.Fatalf("kataCarrierID with appContainerName = %q", got)
	}
	// Older snapshots carry only the workload id.
	if got := kataCarrierID(kataMeta{ContainerID: "app" + kataOverlaySuffix}); got != "app" {
		t.Fatalf("kataCarrierID from containerID = %q", got)
	}
}
