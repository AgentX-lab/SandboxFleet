# SandboxFleet Architecture

## Overview

SandboxFleet has five internal modules:

1. API
2. Controller Manager
3. Slot Scheduler
4. Worker Agent
5. Runtime Adapter

The first four modules manage Sandbox and Slot state. The Runtime Adapter
delegates execution to an existing container runtime.

## API

The API module defines two Kubernetes resources.

### SandboxPool

`SandboxPool` defines Worker capacity:

```yaml
apiVersion: sandboxfleet.io/v1alpha1
kind: SandboxPool
spec:
  workers: 3
  slotsPerWorker: 4
  runtime:
    backend: cri
    handler: runsc
```

It records:

- The desired number of Workers.
- The number of Slots per Worker.
- Slot resource limits.
- The runtime backend and handler.
- Total, used, and available capacity.

### Sandbox

`Sandbox` defines one execution environment:

```yaml
apiVersion: sandboxfleet.io/v1alpha1
kind: Sandbox
spec:
  poolRef: gvisor-pool
  image: python:3.12
  command: ["sleep", "infinity"]
```

Its status records:

- Lifecycle phase.
- Assigned Worker.
- Assigned Slot.
- Runtime handle.
- Last reported error.

The initial phases are `Pending`, `Assigned`, `Starting`, `Running`, `Stopping`,
and `Failed`.

## Controller Manager

The Controller Manager contains the Pool Controller and Sandbox Controller.

### Pool Controller

The Pool Controller:

- Creates and scales Worker Pods for each SandboxPool.
- Passes Slot and runtime configuration to each Worker.
- Tracks Worker health and aggregate Slot capacity.
- Removes Workers during pool scale-down.

Workers should use stable names so that assignments and recovery can identify
the correct Worker after a control-plane restart.

### Sandbox Controller

The Sandbox Controller:

- Watches Sandbox creation, updates, and deletion.
- Requests a Slot assignment from the Slot Scheduler.
- Persists the assignment in Sandbox status.
- Requests the assigned Worker to start or stop the Sandbox.
- Reconciles requested state with actual runtime state.
- Releases the Slot after runtime cleanup succeeds.

The Controller Manager uses Kubernetes leader election. A single active leader
performs Slot assignment.

## Slot Scheduler

The Slot Scheduler owns placement decisions.

It maintains:

- Registered Workers.
- Worker heartbeat state.
- Total and available Slots.
- Current Sandbox-to-Slot assignments.
- Runtime and resource compatibility.

The initial scheduling policy selects a healthy Worker in the requested Pool
with a compatible runtime and an available Slot.

An assignment contains:

```text
Sandbox UID
Worker name
Slot ID
```

The assignment is persisted in Sandbox status. Slots are internal scheduling
units and are not Kubernetes resources.

The Scheduler rebuilds its state from Sandbox resources and Worker reports after
a restart.

## Worker Agent

Each Worker Pod runs one Worker Agent.

The Worker Agent:

- Registers the Worker with the control plane.
- Reports Slot capacity and health.
- Maintains the local Slot-to-Sandbox mapping.
- Reserves and releases Slots.
- Starts and stops Sandboxes through the Runtime Adapter.
- Removes runtime resources before marking a Slot as free.
- Rebuilds local state from runtime labels after a restart.

The Worker Agent exposes idempotent operations:

```text
ReserveSlot
StartSandbox
StopSandbox
ReleaseSlot
GetSandbox
ListSlots
Exec
```

Each runtime object is labeled with its Sandbox UID, Worker name, and Slot ID.
These labels allow state recovery without a separate local database.

## Runtime Adapter

The Runtime Adapter is an internal Worker Agent package. It is not a separate
service and does not implement a container runtime.

It exposes a runtime-neutral interface:

```go
type Runtime interface {
	Create(ctx context.Context, spec SandboxSpec) (Handle, error)
	Start(ctx context.Context, handle Handle) error
	Stop(ctx context.Context, handle Handle) error
	Delete(ctx context.Context, handle Handle) error
	Status(ctx context.Context, handle Handle) (Status, error)
	Exec(ctx context.Context, handle Handle, command []string) error
}
```

`Handle` is opaque to the Slot Scheduler:

```go
type Handle struct {
	ID string
}
```

The Scheduler stores the Handle but does not interpret runtime-specific IDs.

## CRI Backend

The first Runtime implementation is `CRIRuntime`.

`CRIRuntime` uses the Kubernetes CRI v1 client to call a local containerd
instance. It performs:

- Image pull.
- PodSandbox creation.
- Container creation and start.
- Container stop and removal.
- PodSandbox removal.
- Status lookup.
- Command execution.

The mapping is:

| SandboxFleet object | CRI object |
| --- | --- |
| Slot | PodSandbox |
| Sandbox | Container |
| Runtime Handle | PodSandbox ID |

For gVisor, the Pool uses the `runsc` runtime handler. Containerd delegates
execution to `containerd-shim-runsc-v1`, which invokes `runsc`.

SandboxFleet does not import gVisor internals or invoke `runsc` directly.

## Runtime Extensibility

Runtime selection belongs to SandboxPool. A Worker Pool uses one runtime
configuration.

Examples:

```yaml
runtime:
  backend: cri
  handler: runsc
```

```yaml
runtime:
  backend: cri
  handler: kata
```

Optional host devices (for example `/dev/kvm`) are declared on the Pool as
`spec.runtime.cri.hostDevices` and mounted into Worker Pods by the controller.
Runtime selection does not hard-code device requirements by handler name.

Any runtime with a containerd CRI handler can use `CRIRuntime`. Supporting such
a runtime requires a Worker image + Pool config, not a new Slot Scheduler.

A runtime without CRI support requires another Runtime implementation:

```text
internal/runtime/
├── runtime.go
├── cri/
│   └── runtime.go
└── firecracker/
    └── runtime.go
```

The new implementation must satisfy the same Runtime interface. The API,
controllers, Scheduler, and Worker Slot logic remain unchanged.

The initial version supports `CRIRuntime` with `runc`, `runsc` (gVisor), and
`kata` (Cloud Hypervisor) handlers.

## State Ownership

| State | Owner |
| --- | --- |
| Desired Pool configuration | `SandboxPool.spec` |
| Aggregate Pool capacity | `SandboxPool.status` |
| Desired Sandbox configuration | `Sandbox.spec` |
| Sandbox phase and assignment | `Sandbox.status` |
| Placement decisions | Slot Scheduler |
| Local Slot state | Worker Agent |
| Container and process state | containerd |
| gVisor execution state | runsc |

Kubernetes resources are the persistent source of desired state and assignment.
Worker and Scheduler state must be reconstructable.

## Package Layout

```text
api/
└── v1alpha1/

cmd/
├── controller-manager/
└── worker/

internal/
├── controller/
├── scheduler/
├── worker/
└── runtime/
    ├── runtime.go
    └── cri/

config/
├── crd/
├── rbac/
└── manager/

docs/
├── architecture.md
└── design.md
```

## Initial Scope

The initial implementation includes:

- SandboxPool and Sandbox APIs.
- Fixed Slot capacity per Worker.
- Sandbox-to-Slot placement.
- Worker registration and health reporting.
- CRI-based gVisor Sandbox lifecycle.
- Slot cleanup and state recovery.

Checkpointing, migration, mixed runtimes within one Pool, and custom VM
backends are outside the initial scope.
