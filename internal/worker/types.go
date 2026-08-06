package worker

import (
	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
)

// Config is Worker-local SlotManager configuration (not part of the HTTP wire API).
type Config struct {
	Name      string
	Namespace string
	Pool      string
	Slots     []slot.Config
	Runtime   sandboxv1alpha1.RuntimeConfig
}
