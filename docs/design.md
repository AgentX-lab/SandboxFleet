# SandboxFleet: Core Architecture

## Goal

SandboxFleet runs multiple isolated sandboxes as slots inside a shared Kubernetes
Worker Pod.

Kubernetes schedules Worker Pods onto nodes. SandboxFleet schedules Sandboxes
into Slots inside those Workers.

## Core Concepts

### Sandbox

A Sandbox is the user-facing execution environment. It defines the image and
command to run and has a stable identity independent of its assigned Worker or
Slot.

### Worker

A Worker is a long-running Kubernetes Pod that hosts a fixed number of Slots. A
Worker Agent inside the Pod creates, stops, and removes Sandboxes.

### Slot

A Slot is the capacity to run one Sandbox. Each occupied Slot maps to a separate
gVisor sandbox. A Slot is local to its Worker and is not a Kubernetes API
resource.

## Architecture

The control plane contains two components:

- The Sandbox Controller accepts Sandbox requests and tracks their state.
- The Slot Scheduler selects a Worker and a free Slot.

Each Worker Pod contains:

- A Worker Agent that manages Slot lifecycle.
- A fixed set of Slots.
- One independent gVisor sandbox for each occupied Slot.

Users submit Sandboxes through the Kubernetes API. The Sandbox Controller asks
the Slot Scheduler for capacity. The selected Worker Agent starts the Sandbox in
the assigned Slot and reports its state to the control plane.

## Scheduling Model

SandboxFleet uses two scheduling levels:

1. The Kubernetes scheduler places Worker Pods onto nodes.
2. The Slot Scheduler places Sandboxes into Slots inside Worker Pods.

Kubernetes manages aggregate Worker resources. SandboxFleet ensures that the
number and resource usage of assigned Sandboxes do not exceed Worker capacity.

## Sandbox Lifecycle

When a user creates a Sandbox, the Sandbox Controller requests a Slot from the
Slot Scheduler. The scheduler selects a Worker with available capacity. The
Worker Agent starts an isolated gVisor sandbox in the assigned Slot and reports
when it is ready.

When a Sandbox terminates, the Worker Agent:

1. Stops the gVisor sandbox.
2. Removes its processes, filesystem, network, and credentials.
3. Marks the Slot as free.

The Slot can then run another Sandbox.

## API Model

The initial API contains two resources.

### SandboxPool

`SandboxPool` defines the number of Worker Pods and Slots per Worker.

```yaml
apiVersion: sandboxfleet.io/v1alpha1
kind: SandboxPool
spec:
  workers: 3
  slotsPerWorker: 4
```

This pool provides capacity for twelve concurrent Sandboxes.

### Sandbox

`Sandbox` defines one execution environment.

```yaml
apiVersion: sandboxfleet.io/v1alpha1
kind: Sandbox
spec:
  image: python:3.12
  command: ["sleep", "infinity"]
```

Its status records the current assignment:

```yaml
status:
  phase: Running
  worker: worker-1
  slot: 2
```

## Isolation Model

Each Slot runs in a distinct gVisor sandbox with its own process tree,
filesystem, network namespace, and resource limits.

The Worker Agent is trusted. Sandboxes are untrusted. The Worker Pod is the
capacity and failure boundary: if it fails, all Sandboxes assigned to its Slots
are affected.

## MVP Scope

The first version includes:

- A fixed number of Slots per Worker.
- Multiple concurrent gVisor Sandboxes in one Worker Pod.
- Sandbox-to-Slot scheduling.
- Sandbox startup, status reporting, termination, and Slot cleanup.
- Worker capacity reporting.

The first version excludes:

- Process checkpoint and restore.
- Live migration.
- Multiple runtime implementations.
- Automatic environment generation.
- Compatibility layers for other Sandbox APIs.
