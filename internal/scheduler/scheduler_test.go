package scheduler

import (
	"errors"
	"testing"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
)

func TestAssignUsesStableWorkerAndSlotOrder(t *testing.T) {
	s := New(StableStrategy{})
	s.UpdateWorker(worker("ns", "pool", "worker-b", "small", 1, 0))
	s.UpdateWorker(worker("ns", "pool", "worker-a", "small", 2, 0))

	assignment, err := s.Assign(AssignRequest{
		SandboxUID:  "sandbox-1",
		Namespace:   "ns",
		Name:        "sandbox",
		Pool:        "pool",
		SlotProfile: "small",
	})
	if err != nil {
		t.Fatalf("Assign() error = %v", err)
	}
	if assignment.Worker.Name != "worker-a" || assignment.SlotID != 0 {
		t.Fatalf("Assign() = worker %q slot %d, want worker-a slot 0", assignment.Worker.Name, assignment.SlotID)
	}
}

func TestAssignFiltersBySlotProfile(t *testing.T) {
	s := New(StableStrategy{})
	s.UpdateWorker(WorkerState{
		Key:     WorkerKey{Namespace: "ns", Pool: "pool", Name: "worker"},
		Healthy: true,
		Slots: map[int32]slot.Info{
			0: {ID: 0, Profile: "small", State: slot.StateFree},
			1: {ID: 1, Profile: "large", State: slot.StateFree},
		},
	})

	assignment, err := s.Assign(AssignRequest{
		SandboxUID:  "sandbox-1",
		Namespace:   "ns",
		Pool:        "pool",
		SlotProfile: "large",
	})
	if err != nil {
		t.Fatalf("Assign() error = %v", err)
	}
	if assignment.SlotID != 1 || assignment.SlotProfile != "large" {
		t.Fatalf("Assign() = %#v, want large slot 1", assignment)
	}
}

func TestAssignIsIdempotent(t *testing.T) {
	s := New(StableStrategy{})
	s.UpdateWorker(worker("ns", "pool", "worker", "small", 1))
	request := AssignRequest{SandboxUID: "sandbox-1", Namespace: "ns", Name: "sandbox", Pool: "pool", SlotProfile: "small"}

	first, err := s.Assign(request)
	if err != nil {
		t.Fatalf("first Assign() error = %v", err)
	}
	second, err := s.Assign(request)
	if err != nil {
		t.Fatalf("second Assign() error = %v", err)
	}
	if first != second {
		t.Fatalf("second Assign() = %#v, want %#v", second, first)
	}
}

func TestAssignReturnsNoCapacity(t *testing.T) {
	s := New(StableStrategy{})
	s.UpdateWorker(worker("ns", "pool", "worker", "small"))

	_, err := s.Assign(AssignRequest{SandboxUID: "sandbox-1", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Assign() error = %v, want ErrNoCapacity", err)
	}
}

func TestAssignRejectsMissingProfile(t *testing.T) {
	s := New(StableStrategy{})
	s.UpdateWorker(worker("ns", "pool", "worker", "small", 0))
	_, err := s.Assign(AssignRequest{SandboxUID: "sandbox-1", Namespace: "ns", Pool: "pool"})
	if !errors.Is(err, ErrInvalidSlotProfile) {
		t.Fatalf("Assign() error = %v, want ErrInvalidSlotProfile", err)
	}
}

func TestReleaseMakesSlotAvailable(t *testing.T) {
	s := New(StableStrategy{})
	s.UpdateWorker(worker("ns", "pool", "worker", "small", 0))
	first, err := s.Assign(AssignRequest{SandboxUID: "sandbox-1", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if err != nil {
		t.Fatalf("Assign() error = %v", err)
	}
	if err := s.Release(first.SandboxUID); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	second, err := s.Assign(AssignRequest{SandboxUID: "sandbox-2", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if err != nil {
		t.Fatalf("Assign() after Release() error = %v", err)
	}
	if second.SlotID != first.SlotID {
		t.Fatalf("Assign() slot = %d, want released slot %d", second.SlotID, first.SlotID)
	}
}

func TestRestoreBeforeWorkerDiscovery(t *testing.T) {
	s := New(StableStrategy{})
	assignment := Assignment{
		SandboxUID:  "sandbox-1",
		Namespace:   "ns",
		Name:        "sandbox",
		Worker:      WorkerKey{Namespace: "ns", Pool: "pool", Name: "worker"},
		SlotID:      0,
		SlotProfile: "small",
	}
	if err := s.Restore(assignment); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	s.UpdateWorker(worker("ns", "pool", "worker", "small", 0))

	_, err := s.Assign(AssignRequest{SandboxUID: "sandbox-2", Namespace: "ns", Pool: "pool", SlotProfile: "small"})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Assign() error = %v, want restored slot to stay reserved", err)
	}
}

func worker(namespace, pool, name, profile string, slots ...int32) WorkerState {
	slotState := make(map[int32]slot.Info, len(slots))
	for _, id := range slots {
		slotState[id] = slot.Info{ID: id, Profile: profile, State: slot.StateFree}
	}
	return WorkerState{
		Key:     WorkerKey{Namespace: namespace, Pool: pool, Name: name},
		Healthy: true,
		Slots:   slotState,
	}
}
