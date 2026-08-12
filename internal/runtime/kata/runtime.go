// Copyright 2026 The SandboxFleet Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package kata is the Worker Runtime for self-managed Cloud Hypervisor
// micro-VMs. The Worker boots and owns the VMM itself (no kata shim, no CRI pod
// sandbox), so every operation goes through the kata snapshotter's on-disk
// instance records rather than a container runtime API.
package kata

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/snapshotter"
)

type Runtime struct {
	kata *snapshotter.Kata
}

var _ sandboxruntime.Runtime = (*Runtime)(nil)

func New(kata *snapshotter.Kata) *Runtime { return &Runtime{kata: kata} }

// Create cold-boots a micro-VM. ColdBoot is linux-only and returns a clear
// error elsewhere, so this needs no build tag of its own.
func (r *Runtime) Create(ctx context.Context, req sandboxruntime.CreateRequest) (sandboxruntime.ID, error) {
	return r.kata.ColdBoot(ctx, req)
}

// Start is a no-op: Create only returns once the kata-agent reports the
// workload process running.
func (r *Runtime) Start(context.Context, sandboxruntime.ID) error { return nil }

// Stop and Delete both tear the VMM down. A micro-VM cannot be stopped and
// resumed in place — its guest RAM holds the only copy of the sandbox state —
// so stopping is deleting.
func (r *Runtime) Stop(ctx context.Context, id sandboxruntime.ID) error {
	return r.kata.DeleteRestored(ctx, id)
}

func (r *Runtime) Delete(ctx context.Context, id sandboxruntime.ID) error {
	return r.kata.DeleteRestored(ctx, id)
}

func (r *Runtime) Status(_ context.Context, id sandboxruntime.ID) (sandboxruntime.Status, error) {
	instance, err := r.kata.Instance(id)
	if err != nil {
		return sandboxruntime.Status{}, err
	}
	if !processAlive(instance.PID) {
		return sandboxruntime.Status{
			State:   sandboxruntime.StateStopped,
			Message: fmt.Sprintf("cloud-hypervisor (pid %d) is gone", instance.PID),
		}, nil
	}
	return sandboxruntime.Status{State: sandboxruntime.StateRunning}, nil
}

func (r *Runtime) List(context.Context) ([]sandboxruntime.Info, error) {
	instances, err := r.kata.Instances()
	if err != nil {
		return nil, fmt.Errorf("list kata instances: %w", err)
	}
	result := make([]sandboxruntime.Info, 0, len(instances))
	for _, instance := range instances {
		info := sandboxruntime.Info{
			ID:       instance.ID,
			Identity: instance.Identity,
			SlotID:   instance.SlotID,
			Status:   sandboxruntime.Status{State: sandboxruntime.StateRunning},
		}
		if !processAlive(instance.PID) {
			info.Status.State = sandboxruntime.StateStopped
		}
		result = append(result, info)
	}
	return result, nil
}

func (r *Runtime) Exec(ctx context.Context, id sandboxruntime.ID, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	if len(req.Command) == 0 {
		return sandboxruntime.ExecResult{}, errors.New("exec command is required")
	}
	return r.kata.ExecRestored(ctx, id, req)
}

// PrimaryContainerID reports the kata container the workload runs in (the
// overlay workload, not its carrier). Not part of the Runtime interface; the
// Worker type-asserts for it when writing snapshot meta.
func (r *Runtime) PrimaryContainerID(_ context.Context, id sandboxruntime.ID) (string, error) {
	instance, err := r.kata.Instance(id)
	if err != nil {
		return "", err
	}
	return instance.ContainerID, nil
}

// processAlive reports whether the VMM process still exists. Signal 0 only
// probes; it never disturbs the target.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
