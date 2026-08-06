package snapshotter

import (
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
)

func TestNew(t *testing.T) {
	t.Parallel()

	gvisor, err := New(sandboxv1alpha1.SnapshotterGVisor)
	if err != nil || gvisor == nil {
		t.Fatalf("New(gvisor) = (%v, %v)", gvisor, err)
	}
	kata, err := New(sandboxv1alpha1.SnapshotterKata)
	if err != nil || kata == nil {
		t.Fatalf("New(kata) = (%v, %v)", kata, err)
	}
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") should fail")
	}
	if _, err := New("nope"); err == nil {
		t.Fatal("New(\"nope\") should fail")
	}
}
