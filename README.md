# SandboxFleet

SandboxFleet runs multiple isolated AI agent sandboxes as slots inside shared
Kubernetes Worker Pods. Sandboxes are scheduled into fixed-capacity Slots;
occupied Slots run through containerd (for example with a gVisor `runsc`
handler).

Design docs: [docs/design.md](docs/design.md), [docs/architecture.md](docs/architecture.md).

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
  or `runc`). Example: `WORKER_RUNTIME=runc ./hack/deploy-kind.sh` builds only the
  base image and sets `runtimeHandler=runc`.
- `APPLY_SAMPLES=1` (default) also applies demo Pool/Sandbox manifests; e2e uses
  its own namespace and does not depend on those samples.
- `APPLY_SAMPLES=0 ./hack/deploy-kind.sh` installs only the control plane.
- Re-run tests later without rebuilding: `./hack/verify-e2e.sh`
- Build Worker images alone: `./hack/build-worker-images.sh runc|gvisor|all`
