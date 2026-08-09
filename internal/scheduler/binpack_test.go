package scheduler

import (
	"context"
	"testing"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
)

func TestBinPackFillsPrimaryBeforeSecondary(t *testing.T) {
	s := New(BinPackStrategy{})
	// Names match fork e2e Workers: primary (2 slots) then secondary (1).
	s.UpdateWorker(worker("ns", "pool", "fork-pool-secondary-worker-0", "small", 0))
	s.UpdateWorker(worker("ns", "pool", "fork-pool-primary-worker-0", "small", 0, 1))

	parent, err := s.Assign(AssignRequest{SandboxUID: "parent", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	if parent.Worker.Name != "fork-pool-primary-worker-0" {
		t.Fatalf("parent worker = %q, want primary", parent.Worker.Name)
	}

	child, err := s.Assign(AssignRequest{SandboxUID: "child", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	if child.Worker.Name != "fork-pool-primary-worker-0" {
		t.Fatalf("child worker = %q, want primary (bin-pack)", child.Worker.Name)
	}

	grandchild, err := s.Assign(AssignRequest{SandboxUID: "grandchild", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if err != nil {
		t.Fatalf("grandchild: %v", err)
	}
	if grandchild.Worker.Name != "fork-pool-secondary-worker-0" {
		t.Fatalf("grandchild worker = %q, want secondary", grandchild.Worker.Name)
	}
}

func TestBinPackPrefersPartiallyFilledWorker(t *testing.T) {
	s := New(BinPackStrategy{})
	s.UpdateWorker(worker("ns", "pool", "worker-a", "small", 0, 1))
	s.UpdateWorker(worker("ns", "pool", "worker-b", "small", 0, 1))

	first, err := s.Assign(AssignRequest{SandboxUID: "s1", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Mark first worker as partially filled via a second UpdateWorker view is
	// unnecessary: Assign already reserved the slot in memory.
	second, err := s.Assign(AssignRequest{SandboxUID: "s2", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Worker.Name != first.Worker.Name {
		t.Fatalf("second worker = %q, want same as first %q", second.Worker.Name, first.Worker.Name)
	}
}

func TestBinPackSelectEmpty(t *testing.T) {
	_, err := (BinPackStrategy{}).Select(context.Background(), AssignRequest{}, nil)
	if err != ErrNoCapacity {
		t.Fatalf("error = %v, want ErrNoCapacity", err)
	}
}

func TestCountWorkerSlots(t *testing.T) {
	free, busy := countWorkerSlots(map[int32]slot.Info{
		0: {ID: 0, Profile: "small", State: slot.StateFree},
		1: {ID: 1, Profile: "small", State: slot.StateRunning},
		2: {ID: 2, Profile: "large", State: slot.StateFree},
	}, "small")
	if free != 1 || busy != 1 {
		t.Fatalf("free=%d busy=%d, want 1/1", free, busy)
	}
}
