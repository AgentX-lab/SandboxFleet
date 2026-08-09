package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"k8s.io/apimachinery/pkg/types"
)

var (
	ErrNoCapacity          = errors.New("no available slot")
	ErrAssignmentConflict  = errors.New("assignment conflicts with slot ownership")
	ErrInvalidSlotProfile  = errors.New("invalid slot profile")
	ErrNoStrategyCandidate = errors.New("strategy returned no candidate")
)

type WorkerKey struct {
	Namespace string
	Pool      string
	Name      string
}

// WorkerState is the Scheduler's current view of one Worker.
type WorkerState struct {
	Key          WorkerKey
	Healthy      bool
	LastObserved time.Time
	Slots        map[int32]slot.Info
}

// Assignment records the Worker and Slot assigned to one Sandbox.
type Assignment struct {
	SandboxUID  types.UID
	Namespace   string
	Name        string
	Worker      WorkerKey
	SlotID      int32
	SlotProfile string
}

type AssignRequest struct {
	SandboxUID  types.UID
	Namespace   string
	Name        string
	Pool        string
	SlotProfile string
}

// Candidate is one schedulable free Slot after hard filters.
type Candidate struct {
	Worker  WorkerKey
	SlotID  int32
	Profile string
	// FreeSlots is how many matching-profile free slots this Worker has.
	FreeSlots int
	// BusySlots is how many non-free slots this Worker has (any profile).
	BusySlots int
}

// Strategy chooses one Candidate from the filtered set.
type Strategy interface {
	Name() string
	Select(ctx context.Context, req AssignRequest, candidates []Candidate) (Candidate, error)
}

type Scheduler interface {
	UpdateWorker(state WorkerState)
	RemoveWorker(key WorkerKey)
	Assign(req AssignRequest) (Assignment, error)
	Restore(assignment Assignment) error
	Release(sandboxUID types.UID) error
	Get(sandboxUID types.UID) (Assignment, bool)
}
