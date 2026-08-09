package snapshotter

import "context"

// CRICleanup is an optional capability for Snapshotter adapters.
//
// After leave-running checkpoint, leftover runtime processes may keep the CRI
// pod's CNI netns busy so StopPodSandbox fails with EBUSY. Implementations
// should best-effort tear down those holders; errors are ignored by callers.
type CRICleanup interface {
	BestEffortCleanupCRI(ctx context.Context, podSandboxID string)
}
