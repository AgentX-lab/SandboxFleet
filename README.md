# SandboxFleet

* Multiplexing: Many isolated AI agent sandboxes share one Kubernetes Worker Pod, each occupying a fixed-capacity Slot.
* Scheduling: A Slot Scheduler places each Sandbox onto a matching Slot (Slots can have different resource profiles).
* Fork: A running Sandbox can be snapshotted and forked into child Sandboxes (including nested forks); each child is scheduled into its own Slot.

<img width="1626" height="1072" alt="image" src="https://github.com/user-attachments/assets/5367ec02-b71a-4ef3-8cf3-091c846caf1a" />


## Prerequisites

Install on your machine:

- `docker`
- `kind`
- `kubectl`
- `go` (1.26+)

## Quick start (deploy + verify)

From the repository root:

```bash
# 1. Create a kind cluster, build images, and install SandboxFleet
./hack/deploy-kind.sh

# 2. Run end-to-end tests against that cluster (does not redeploy)
./hack/verify-e2e.sh
```

What this checks: create a Pool, create a Sandbox, run a command via Exec, then
delete the Sandbox.

Optional cleanup:

```bash
./hack/cleanup-kind.sh
```

## Notes

- `deploy-kind.sh` writes kubeconfig to `bin/KUBECONFIG` and runtime selection to
  `bin/runtime.env` (used by `verify-e2e.sh`).
- `WORKER_RUNTIME` selects which Worker image to build and load (`gvisor` default,
  `runc`, or `kata`). It only picks image + `runtimeHandler` (+ optional sample
  `hostDevices`). Nested virt is handled by `hack/ensure-kind-cluster.sh` via
  `ENSURE_NESTED_VIRT=auto|1|0` (independent of runtime name).
  Kata pools declare `spec.runtime.cri.hostDevices: ["/dev/kvm"]`.
- `APPLY_SAMPLES=1` (default) also applies demo Pool/Sandbox manifests; e2e uses
  its own namespace and does not depend on those samples.
- `APPLY_SAMPLES=0 ./hack/deploy-kind.sh` installs only the control plane.
- Re-run tests later without rebuilding: `./hack/verify-e2e.sh`
- Build Worker images alone: `./hack/build-worker-images.sh runc|gvisor|kata|all`
