package slot

import "k8s.io/apimachinery/pkg/types"

type State string

const (
	StateFree     State = "Free"
	StateReserved State = "Reserved"
	StateStarting State = "Starting"
	StateRunning  State = "Running"
	StateStopping State = "Stopping"
	StateCleaning State = "Cleaning"
	StateFailed   State = "Failed"
)

// Info is the state shared between a Worker and the Scheduler.
type Info struct {
	ID         int32     `json:"id"`
	State      State     `json:"state"`
	SandboxUID types.UID `json:"sandboxUID,omitempty"`
}
