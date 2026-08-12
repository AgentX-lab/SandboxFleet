//go:build !linux

package snapshotter

import (
	"context"
	"fmt"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

func (k *Kata) ColdBoot(context.Context, sandboxruntime.CreateRequest) (sandboxruntime.ID, error) {
	return sandboxruntime.ID{}, fmt.Errorf("kata cold boot is only supported on linux")
}
