package snapshotter

import (
	"fmt"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
)

// New returns the memory checkpoint/restore adapter for kind.
func New(kind sandboxv1alpha1.SnapshotterKind) (Snapshotter, error) {
	switch kind {
	case sandboxv1alpha1.SnapshotterGVisor:
		return NewGVisor(), nil
	case sandboxv1alpha1.SnapshotterKata:
		return NewKata(), nil
	case "":
		return nil, fmt.Errorf("snapshotter is required (gvisor or kata)")
	default:
		return nil, fmt.Errorf("unknown snapshotter %q (want gvisor or kata)", kind)
	}
}
