# SandboxFleet Component Design

## 1. Design Principles

State ownership:

- Kubernetes API stores desired state and assignment.
- Scheduler stores only reconstructable assignment cache.
- Worker stores local Slot state.
- Runtime Adapter stores no business state; containerd owns runtime objects.
- Controller owns no durable business state.

Coupling rules:

- Dependencies point inward through interfaces only.
- Runtime types never appear in the Kubernetes API.
- Controller never imports CRI packages.
- Scheduler never imports Worker or Runtime packages.
- Cross-component calls are idempotent.
- A Slot cannot be reused until runtime cleanup succeeds.

All resources are namespace-scoped. A Sandbox may reference only a SandboxPool in
the same namespace.

Shared Slot states used by Scheduler and Worker:

```text
Free | Reserved | Starting | Running | Stopping | Cleaning | Failed
```

`SlotState` lives in a shared internal package imported by both.

## 2. API Component

### 2.1 SandboxPool

```go
type SandboxPoolSpec struct {
	Workers        int32
	SlotsPerWorker int32
	SlotResources  corev1.ResourceRequirements
	Runtime        RuntimeConfig
}

type RuntimeConfig struct {
	Backend RuntimeBackend
	CRI     *CRIRuntimeConfig
}

type CRIRuntimeConfig struct {
	// RuntimeHandler is the CRI runtime name, such as "runsc".
	RuntimeHandler string
}

type SandboxPoolStatus struct {
	ObservedGeneration int64
	CurrentWorkers      int32
	ReadyWorkers        int32
	UsedSlots           int32
	AvailableSlots      int32
	Conditions          []metav1.Condition
}
```

Validation:

- `workers >= 0`
- `slotsPerWorker > 0`
- MVP allows only `backend=cri`; `cri.runtimeHandler` is an opaque non-empty string
- `cri` must be set when `backend=cri`
- `slotsPerWorker` and `runtime` are immutable

Conditions:

- `Ready`: enough healthy Workers exist to serve the Pool
- `WorkersReady`: expected Workers are healthy

`Ready` depends only on Worker health, not on scale-down or free capacity.
Capacity is read from `availableSlots`.

Derived values:

```text
totalSlots = currentWorkers * Spec.SlotsPerWorker
unavailableSlots = totalSlots - usedSlots - availableSlots
```

A Pool is homogeneous: every Worker uses the same runtime and Slot capacity.

### 2.2 Sandbox

```go
type SandboxSpec struct {
	PoolRef   string
	Container ContainerSpec
}

type ContainerSpec struct {
	Image   string
	Command []string
	Args    []string
	Env     []corev1.EnvVar
}

type SandboxStatus struct {
	ObservedGeneration int64
	Phase              SandboxPhase
	Assignment         *Assignment
	Conditions         []metav1.Condition
}

// Assignment identifies the Worker and Slot assigned to this Sandbox.
type Assignment struct {
	Worker string
	SlotID int32
}

type SandboxPhase string
```

Validation:

- `poolRef` required and immutable
- `container.image` required
- `assignment` writable only through status

Phases:

- `Pending`: waiting for assignment
- `Starting`: assigned and starting on the Worker
- `Running`: container is running
- `Stopping`: cleanup in progress
- `Failed`: cannot make progress

Conditions drive reconciliation:

- `Scheduled`: assignment is persisted
- `Ready`: Sandbox is running on the assigned Worker

Deletion is tracked by the finalizer, not by a Condition.

`Phase` is a display summary. Decisions use Conditions and `Assignment`.

Every Sandbox has a finalizer. Remove it only after runtime cleanup and Slot
release succeed.

API `Assignment` maps from Scheduler `SchedulerAssignment` as:

```text
Assignment.Worker = SchedulerAssignment.Worker.Name
Assignment.SlotID = SchedulerAssignment.SlotID
```

## 3. Controller Manager

One process hosts Pool Controller, Sandbox Controller, and Scheduler.
Leader election ensures one active reconciler.

| Component | Owns |
| --- | --- |
| Pool Controller | Worker Pods, Pool status, Scheduler Worker inventory |
| Sandbox Controller | Sandbox status, Assign/Release, Worker lifecycle calls |
| Scheduler | Assignment decisions only |

### 3.1 Pool Controller

```go
type PoolReconciler struct {
	Client    client.Client
	Scheme    *runtime.Scheme
	Scheduler Scheduler
}
```

Worker image and port are controller process flags, not Pool API fields.

Flow:

1. Ensure one StatefulSet per SandboxPool.
2. Use stable Worker names: `<pool-name>-worker-<ordinal>`.
3. Resolve Worker endpoints from Pods for WorkerClient calls.
4. Pull health and `ListSlots`.
5. `Scheduler.UpdateWorker` / `RemoveWorker`.
6. Write Pool status.
7. Block unsafe scale-down without changing `Ready`.

Worker endpoints stay in the Controller. Scheduler does not store them.

Worker config injected from the SandboxPool:

```go
type WorkerConfig struct {
	Name          string
	Namespace     string
	Pool          string
	Slots         int32
	SlotResources corev1.ResourceRequirements
	Runtime       RuntimeConfig
}
```

Registration is pull-based. Workers do not call the control plane.

Scale-down targets the highest ordinal. If that Worker has a non-free Slot, keep
the StatefulSet unchanged. Other Workers with free Slots remain schedulable.

### 3.2 Sandbox Controller

```go
type SandboxReconciler struct {
	Client       client.Client
	Scheduler    Scheduler
	WorkerClient WorkerClient
}
```

Create:

1. Ensure Pool exists and is Ready.
2. `Scheduler.Assign`.
3. Persist `Assignment`, set `Scheduled=True`, phase `Starting`.
4. `Worker.ReserveSlot`.
5. `Worker.StartSandbox`.
6. Set `Ready=True`, phase `Running`.

Delete:

1. Set phase `Stopping`.
2. `Worker.StopSandbox`.
3. `Worker.ReleaseSlot`.
4. `Scheduler.Release`.
5. Remove finalizer.

Persist assignment before calling the Worker. Runtime IDs stay inside the Worker.

If start succeeds but status update fails, the next reconcile reads Worker Slot
state and finishes status without creating another runtime.

## 4. Slot Scheduler

### 4.1 State

```go
type scheduler struct {
	lock        sync.RWMutex
	workers     map[WorkerKey]*WorkerState
	assignments map[types.UID]SchedulerAssignment
}

type WorkerKey struct {
	Namespace string
	Pool      string
	Name      string
}

// WorkerState is the Scheduler's view of one Worker.
type WorkerState struct {
	Key          WorkerKey
	Healthy      bool
	LastObserved time.Time
	Slots        map[int32]SlotInfo
}

// SlotInfo is the Scheduler's view of one Slot.
type SlotInfo struct {
	ID         int32
	State      SlotState
	SandboxUID types.UID
}

// SchedulerAssignment records the Worker and Slot assigned to a Sandbox.
type SchedulerAssignment struct {
	SandboxUID types.UID
	Namespace  string
	Name       string
	Worker     WorkerKey
	SlotID     int32
}
```

`scheduler` implements `Scheduler`. The Controller builds a `WorkerState` from
health and `ListSlots`, then calls `UpdateWorker`.

### 4.2 Interface

```go
type Scheduler interface {
	UpdateWorker(state WorkerState)
	RemoveWorker(key WorkerKey)
	Assign(req AssignRequest) (SchedulerAssignment, error)
	Restore(assignment SchedulerAssignment) error
	Release(sandboxUID types.UID) error
	Get(sandboxUID types.UID) (SchedulerAssignment, bool)
}

type AssignRequest struct {
	SandboxUID types.UID
	Namespace  string
	Name       string
	Pool       string
}
```

`Assign` matches on `SandboxUID` and `Pool`. `Namespace` and `Name` are stored
for recovery and logging only.

### 4.3 Assignment Rules

Select the first Slot where:

1. Worker is healthy.
2. Worker belongs to the requested Pool.
3. Slot is free.

Workers in one Pool already share the same runtime, so Assign does not compare
runtime configs.

Sort Workers by name and Slots by ID. `Assign` is locked and idempotent for the
same Sandbox UID.

### 4.4 Recovery

1. Restore assignments from Sandbox status.
2. Discover Workers and read `ListSlots`.
3. Reconcile cache against actual Slot ownership.

Scheduler cache is never authoritative.

## 5. Worker Agent

### 5.1 State

```go
type SlotManager struct {
	config  WorkerConfig
	runtime Runtime
	slots   map[int32]*Slot
}

type Slot struct {
	lock       sync.Mutex
	ID         int32
	State      SlotState
	SandboxUID types.UID
	RuntimeRef *RuntimeID
}
```

Each Slot has its own lock. Different Slots may run concurrently.

### 5.2 Client Contract

```go
type WorkerClient interface {
	Health(ctx context.Context, endpoint string) error
	ListSlots(ctx context.Context, endpoint string) ([]SlotInfo, error)
	ReserveSlot(ctx context.Context, endpoint string, req SandboxSlotRef) error
	StartSandbox(ctx context.Context, endpoint string, req StartSandboxRequest) error
	StopSandbox(ctx context.Context, endpoint string, req SandboxSlotRef) error
	ReleaseSlot(ctx context.Context, endpoint string, req SandboxSlotRef) error
	GetSandbox(ctx context.Context, endpoint string, req SandboxSlotRef) (SandboxInfo, error)
}

type SandboxIdentity struct {
	Namespace string
	Name      string
	UID       types.UID
}

// SandboxSlotRef identifies one Sandbox on one Slot.
type SandboxSlotRef struct {
	SlotID   int32
	Identity SandboxIdentity
}

type StartSandboxRequest struct {
	SlotID    int32
	Identity  SandboxIdentity
	Container ContainerSpec
}

// SandboxInfo is the Worker's report for one Sandbox.
type SandboxInfo struct {
	Identity SandboxIdentity
	SlotID   int32
	State    SlotState
}

// SlotInfo is the Worker's report for one Slot.
type SlotInfo struct {
	ID         int32
	State      SlotState
	SandboxUID types.UID
}
```

`SlotInfo` is shared by Scheduler and Worker reports because both describe the
same Slot facts.

All calls take `endpoint` as a Controller-resolved argument. Responses expose
Slot state only, never Runtime IDs.

UID ownership:

- Free Slot may be reserved.
- Same UID re-reserve succeeds.
- Other UID returns conflict.
- Stop and release require matching UID.

### 5.3 HTTP API

```text
GET  /healthz
GET  /v1/slots
GET  /v1/slots/{slotID}
POST /v1/slots/{slotID}/reserve
POST /v1/slots/{slotID}/start
POST /v1/slots/{slotID}/stop
POST /v1/slots/{slotID}/release
```

Errors:

- `400 InvalidRequest`
- `404 SandboxNotFound`
- `409 SlotConflict`
- `429 WorkerBusy`
- `500 RuntimeError`
- `503 WorkerUnavailable`

Retry `429`, `500`, and `503`. Resolve `409` by reconciling assignment and Slot
state.

### 5.4 Recovery

No local database. On startup:

1. `Runtime.List`
2. Rebuild Slots from SandboxFleet labels
3. Mark invalid ownership as `Failed`
4. Expose state through `ListSlots`

## 6. Runtime Adapter

Runtime is an internal Worker package. Only `SlotManager` calls it.

### 6.1 Types

```go
type CreateRequest struct {
	Identity  SandboxIdentity
	SlotID    int32
	Container ContainerConfig
}

type ContainerConfig struct {
	Image   string
	Command []string
	Args    []string
	Env     []corev1.EnvVar
}

// RuntimeID is an opaque ID returned by the Runtime Adapter.
type RuntimeID struct {
	ID string
}

type RuntimeStatus struct {
	State    RuntimeState
	ExitCode int32
	Message  string
}
```

`SlotManager` converts API `ContainerSpec` into internal `ContainerConfig`.
Worker name, Slot resources, and CRI `RuntimeHandler` are injected when the
Runtime Adapter is created.

`SandboxIdentity` is required because runtime objects must be associated with a
specific Sandbox:

- `UID` is the unique ownership and idempotency key.
- `Namespace` and `Name` support labels, diagnostics, and recovery.

### 6.2 Interface

```go
type Runtime interface {
	Create(ctx context.Context, req CreateRequest) (RuntimeID, error)
	Start(ctx context.Context, id RuntimeID) error
	Stop(ctx context.Context, id RuntimeID) error
	Delete(ctx context.Context, id RuntimeID) error
	Status(ctx context.Context, id RuntimeID) (RuntimeStatus, error)
	List(ctx context.Context) ([]RuntimeInfo, error)
}
```

`RuntimeID` is opaque outside Runtime. For CRI, `RuntimeID.ID` is the
PodSandbox ID. The Container is found by labels.

### 6.3 CRI Mapping

Labels:

```text
sandboxfleet.io/sandbox-uid
sandboxfleet.io/sandbox-namespace
sandboxfleet.io/sandbox-name
sandboxfleet.io/worker
sandboxfleet.io/slot-id
```

Create:

1. Pull image if needed.
2. Create PodSandbox with configured `RuntimeHandler`.
3. Create one Container.
4. Return PodSandbox ID as `RuntimeID`.

Cleanup by Runtime:

1. Stop Container
2. Remove Container
3. Stop PodSandbox
4. Remove PodSandbox

After Runtime cleanup succeeds, `SlotManager` clears `RuntimeRef` and sets Slot
state to `Free`. Missing objects during cleanup count as already deleted.

## 7. Failure Semantics

- Controller restart: restore assignments from Sandbox status, then verify Worker
  Slot state before acting.
- Worker restart: rebuild Slots from CRI labels.
- Worker unavailable: keep assignment; after timeout mark Sandbox `Failed`.
  MVP does not migrate.
- Runtime missing on healthy Worker: return to `Starting` and recreate in the
  same Slot.
- Partial start failure: clean created objects, return Slot to `Reserved`, retry.
- Cleanup failure: keep Slot `Cleaning` or `Failed`; never mark available.
- Pool scale-down: block while highest-ordinal Worker has a non-free Slot;
  do not mark the whole Pool not Ready.

## 8. Go SDK

The Go SDK is the public client for creating and managing Sandboxes. It wraps
the Kubernetes API and contains no scheduling or runtime logic.

```go
type Client interface {
	CreateSandbox(ctx context.Context, opts CreateOptions) (*v1alpha1.Sandbox, error)
	GetSandbox(ctx context.Context, namespace, name string) (*v1alpha1.Sandbox, error)
	WaitSandboxReady(ctx context.Context, namespace, name string) (*v1alpha1.Sandbox, error)
	DeleteSandbox(ctx context.Context, namespace, name string) error
	WaitSandboxDeleted(ctx context.Context, namespace, name string) error
}

type CreateOptions struct {
	Namespace string
	Name      string
	PoolRef   string
	Container v1alpha1.ContainerSpec
}
```

Rules:

- Use `context.Context` for cancellation and timeouts.
- Wait for `Ready=True`; return an error when the Sandbox reaches `Failed`.
- Preserve Kubernetes API errors.
- Depend only on Kubernetes clients and `api/v1alpha1`.
- Do not import Controller, Scheduler, Worker, or Runtime packages.

E2E tests use this SDK to create, wait for, and delete Sandboxes.

## 9. Package Boundaries

```text
api/v1alpha1/
clients/go/sandboxfleet/
cmd/controller-manager/
cmd/worker/
internal/controller/
internal/scheduler/
internal/worker/
internal/worker/httpapi/
internal/runtime/
internal/runtime/cri/
internal/slot/
```

Dependency direction:

```text
Go SDK -> Kubernetes API
API <- Controller -> Scheduler
Controller -> WorkerClient
Worker HTTP API -> SlotManager
SlotManager -> Runtime
CRI Runtime -> CRI Client
Scheduler/Worker -> shared SlotState
```

Forbidden imports:

- `scheduler` must not import `worker` or `runtime`
- `controller` must not import `runtime/cri`
- `api` must not contain Runtime IDs or CRI types
- `clients/go/sandboxfleet` must not import internal packages

## 10. Open Decisions

1. Whether Sandbox container fields are mutable after creation.
2. Worker HTTP authentication and network isolation.
3. Worker health interval and unavailable timeout.
4. Retry count and backoff policy.
5. Whether fixed Pool Slot resources are enough for MVP.
