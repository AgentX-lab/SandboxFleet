#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-sandboxfleet}"
CONTROLLER_IMAGE="${CONTROLLER_IMAGE:-sandboxfleet-controller:latest}"
BASE_IMAGE="${BASE_IMAGE:-sandboxfleet-worker-base:latest}"
GVISOR_IMAGE="${GVISOR_IMAGE:-sandboxfleet-worker-gvisor:latest}"
KATA_IMAGE="${KATA_IMAGE:-sandboxfleet-worker-kata:latest}"
WORKER_RUNTIME="${WORKER_RUNTIME:-gvisor}"
APPLY_SAMPLES="${APPLY_SAMPLES:-1}"
ENSURE_NESTED_VIRT="${ENSURE_NESTED_VIRT:-auto}"

need() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "missing required command: $1" >&2
		exit 1
	}
}

need docker
need kind
need kubectl

# Runtime catalog: image, handler, sample manifest.
# HOST_DEVICES only drives ENSURE_NESTED_VIRT when auto (cluster capability).
case "${WORKER_RUNTIME}" in
runc)
	WORKER_IMAGE="${WORKER_IMAGE:-${BASE_IMAGE}}"
	RUNTIME_HANDLER=runc
	HOST_DEVICES=
	SAMPLE_POOL="${ROOT}/config/samples/sandboxpool-runc.yaml"
	BUILD_TARGET=runc
	;;
gvisor)
	WORKER_IMAGE="${WORKER_IMAGE:-${GVISOR_IMAGE}}"
	RUNTIME_HANDLER=runsc
	HOST_DEVICES=
	SAMPLE_POOL="${ROOT}/config/samples/sandboxpool.yaml"
	BUILD_TARGET=gvisor
	;;
kata)
	WORKER_IMAGE="${WORKER_IMAGE:-${KATA_IMAGE}}"
	RUNTIME_HANDLER=kata
	HOST_DEVICES=/dev/kvm
	SAMPLE_POOL="${ROOT}/config/samples/sandboxpool-kata.yaml"
	BUILD_TARGET=kata
	;;
*)
	echo "unsupported WORKER_RUNTIME=${WORKER_RUNTIME} (want runc|gvisor|kata)" >&2
	exit 1
	;;
esac

if [[ "${ENSURE_NESTED_VIRT}" == "auto" && "${HOST_DEVICES}" == *"/dev/kvm"* ]]; then
	ENSURE_NESTED_VIRT=1
fi

cd "${ROOT}"
mkdir -p "${ROOT}/bin"

CLUSTER_NAME="${CLUSTER_NAME}" ENSURE_NESTED_VIRT="${ENSURE_NESTED_VIRT}" \
	"${ROOT}/hack/ensure-kind-cluster.sh"

echo "Building controller image ${CONTROLLER_IMAGE}..."
docker build -f build/controller/Dockerfile -t "${CONTROLLER_IMAGE}" "${ROOT}"

echo "Building Worker images for runtime=${WORKER_RUNTIME}..."
BASE_IMAGE="${BASE_IMAGE}" GVISOR_IMAGE="${GVISOR_IMAGE}" KATA_IMAGE="${KATA_IMAGE}" \
	"${ROOT}/hack/build-worker-images.sh" "${BUILD_TARGET}"

echo "Loading images into kind..."
kind load docker-image "${CONTROLLER_IMAGE}" --name "${CLUSTER_NAME}"
kind load docker-image "${WORKER_IMAGE}" --name "${CLUSTER_NAME}"

echo "Applying SandboxFleet manifests (worker-image=${WORKER_IMAGE})..."
kubectl --context "kind-${CLUSTER_NAME}" kustomize "${ROOT}/config" |
	sed "s|--worker-image=.*|--worker-image=${WORKER_IMAGE}|" |
	kubectl --context "kind-${CLUSTER_NAME}" apply -f -

kind get kubeconfig --name "${CLUSTER_NAME}" > "${ROOT}/bin/KUBECONFIG"
cat >"${ROOT}/bin/runtime.env" <<EOF
WORKER_RUNTIME=${WORKER_RUNTIME}
WORKER_IMAGE=${WORKER_IMAGE}
RUNTIME_HANDLER=${RUNTIME_HANDLER}
E2E_RUNTIME_HANDLER=${RUNTIME_HANDLER}
EOF
echo "Wrote ${ROOT}/bin/KUBECONFIG"
echo "Wrote ${ROOT}/bin/runtime.env"

echo "Waiting for controller..."
kubectl --context "kind-${CLUSTER_NAME}" -n sandboxfleet-system \
	rollout status deployment/sandboxfleet-controller --timeout=180s

if [[ "${APPLY_SAMPLES}" == "1" ]]; then
	echo "Applying sample ${SAMPLE_POOL}..."
	kubectl --context "kind-${CLUSTER_NAME}" apply -f "${SAMPLE_POOL}"
	echo "Waiting for Worker Pod..."
	kubectl --context "kind-${CLUSTER_NAME}" -n default wait \
		--for=condition=Ready pod -l sandboxfleet.io/pool=demo --timeout=300s || true
	echo "Applying sample Sandbox..."
	kubectl --context "kind-${CLUSTER_NAME}" apply -f "${ROOT}/config/samples/sandbox.yaml"
	echo
	echo "Check status with:"
	echo "  kubectl get sfp,sf -A"
	echo "  kubectl get pods -l sandboxfleet.io/managed=true"
fi

echo "Deploy complete (context: kind-${CLUSTER_NAME}, runtime=${WORKER_RUNTIME})."
