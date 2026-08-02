# SandboxFleet

SandboxFleet runs multiple isolated AI agent sandboxes as slots inside shared
Kubernetes Worker Pods.

Kubernetes schedules Worker Pods onto nodes. SandboxFleet schedules Sandboxes
into fixed-capacity Slots inside those Workers. Each occupied Slot runs as an
independent gVisor sandbox through the standard containerd runtime stack.

SandboxFleet focuses on Slot capacity, placement, lifecycle, and cleanup. It
delegates image management and sandbox creation to containerd and gVisor.

See the [core concepts](docs/design.md) and
[architecture](docs/architecture.md) for the design.
