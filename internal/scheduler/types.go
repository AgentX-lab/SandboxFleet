package scheduler

import (
	"errors"
	"time"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"k8s.io/apimachinery/pkg/types"
)

var (
	ErrNoCapacity         = errors.New("no available slot")
	ErrAssignmentConflict = errors.New("assignment conflicts with slot ownership")
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
	SandboxUID types.UID
	Namespace  string
	Name       string
	Worker     WorkerKey
	SlotID     int32
}

type AssignRequest struct {
	SandboxUID types.UID
	Namespace  string
	Name       string
	Pool       string
}

type Scheduler interface {
	UpdateWorker(state WorkerState)
	RemoveWorker(key WorkerKey)
	Assign(req AssignRequest) (Assignment, error)
	Restore(assignment Assignment) error
	Release(sandboxUID types.UID) error
	Get(sandboxUID types.UID) (Assignment, bool)
}
