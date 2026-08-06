//go:build !linux

package snapshotter

import (
	"context"
	"fmt"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

func (k *Kata) saveRestoredVMSnapshot(context.Context, SaveRequest) error {
	return fmt.Errorf("kata nested snapshot is only supported on linux")
}

func (k *Kata) LoadSnapshot(context.Context, LoadRequest) (sandboxruntime.ID, error) {
	return sandboxruntime.ID{}, fmt.Errorf("kata memory restore is only supported on linux")
}

func (k *Kata) DeleteRestored(context.Context, sandboxruntime.ID) error { return nil }

func (k *Kata) ExecRestored(context.Context, sandboxruntime.ID, sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	return sandboxruntime.ExecResult{}, fmt.Errorf("kata restored exec is only supported on linux")
}
