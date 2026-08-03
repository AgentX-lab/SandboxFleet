package scheduler

import (
	"errors"
	"testing"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
)

func TestAssignUsesStableWorkerAndSlotOrder(t *testing.T) {
	s := New()
	s.UpdateWorker(worker("ns", "pool", "worker-b", 1, 0))
	s.UpdateWorker(worker("ns", "pool", "worker-a", 2, 0))

	assignment, err := s.Assign(AssignRequest{
		SandboxUID: "sandbox-1",
		Namespace:  "ns",
		Name:       "sandbox",
		Pool:       "pool",
	})
	if err != nil {
		t.Fatalf("Assign() error = %v", err)
	}
	if assignment.Worker.Name != "worker-a" || assignment.SlotID != 0 {
		t.Fatalf("Assign() = worker %q slot %d, want worker-a slot 0", assignment.Worker.Name, assignment.SlotID)
	}
}

func TestAssignIsIdempotent(t *testing.T) {
	s := New()
	s.UpdateWorker(worker("ns", "pool", "worker", 1))
	request := AssignRequest{SandboxUID: "sandbox-1", Namespace: "ns", Name: "sandbox", Pool: "pool"}

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
	s := New()
	s.UpdateWorker(worker("ns", "pool", "worker"))

	_, err := s.Assign(AssignRequest{SandboxUID: "sandbox-1", Namespace: "ns", Pool: "pool"})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Assign() error = %v, want ErrNoCapacity", err)
	}
}

func TestReleaseMakesSlotAvailable(t *testing.T) {
	s := New()
	s.UpdateWorker(worker("ns", "pool", "worker", 0))
	first, err := s.Assign(AssignRequest{SandboxUID: "sandbox-1", Namespace: "ns", Pool: "pool"})
	if err != nil {
		t.Fatalf("Assign() error = %v", err)
	}
	if err := s.Release(first.SandboxUID); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	second, err := s.Assign(AssignRequest{SandboxUID: "sandbox-2", Namespace: "ns", Pool: "pool"})
	if err != nil {
		t.Fatalf("Assign() after Release() error = %v", err)
	}
	if second.SlotID != first.SlotID {
		t.Fatalf("Assign() slot = %d, want released slot %d", second.SlotID, first.SlotID)
	}
}

func TestRestoreBeforeWorkerDiscovery(t *testing.T) {
	s := New()
	assignment := Assignment{
		SandboxUID: "sandbox-1",
		Namespace:  "ns",
		Name:       "sandbox",
		Worker:     WorkerKey{Namespace: "ns", Pool: "pool", Name: "worker"},
		SlotID:     0,
	}
	if err := s.Restore(assignment); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	s.UpdateWorker(worker("ns", "pool", "worker", 0))

	_, err := s.Assign(AssignRequest{SandboxUID: "sandbox-2", Namespace: "ns", Pool: "pool"})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("Assign() error = %v, want restored slot to stay reserved", err)
	}
}

func worker(namespace, pool, name string, slots ...int32) WorkerState {
	slotState := make(map[int32]slot.Info, len(slots))
	for _, id := range slots {
		slotState[id] = slot.Info{ID: id, State: slot.StateFree}
	}
	return WorkerState{
		Key:     WorkerKey{Namespace: namespace, Pool: pool, Name: name},
		Healthy: true,
		Slots:   slotState,
	}
}
