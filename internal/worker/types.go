package worker

import (
	"errors"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"k8s.io/apimachinery/pkg/types"
)

var (
	ErrInvalidRequest  = errors.New("invalid request")
	ErrSlotNotFound    = errors.New("slot not found")
	ErrSandboxNotFound = errors.New("sandbox not found")
	ErrSlotConflict    = errors.New("slot belongs to another sandbox")
	ErrSlotConfigInvalid = errors.New("slot config update rejected")
)

type Config struct {
	Name      string
	Namespace string
	Pool      string
	Slots     []slot.Config
	Runtime   sandboxv1alpha1.RuntimeConfig
}

type SandboxIdentity struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
}

type SandboxSlotRef struct {
	SlotID   int32           `json:"slotID"`
	Identity SandboxIdentity `json:"identity"`
}

type StartSandboxRequest struct {
	SlotID    int32                         `json:"slotID"`
	Identity  SandboxIdentity               `json:"identity"`
	Container sandboxv1alpha1.ContainerSpec `json:"container"`
}

type SandboxInfo struct {
	Identity SandboxIdentity `json:"identity"`
	SlotID   int32           `json:"slotID"`
	State    slot.State      `json:"state"`
}

type ExecSandboxRequest struct {
	SlotID         int32           `json:"slotID"`
	Identity       SandboxIdentity `json:"identity"`
	Command        []string        `json:"command"`
	TimeoutSeconds int64           `json:"timeoutSeconds,omitempty"`
}

type ExecSandboxResult struct {
	ExitCode int32  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}
