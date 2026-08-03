#!/usr/bin/env bash
# Ensure a kind cluster exists.
# ENSURE_NESTED_VIRT=auto|1|0 — mount /dev/kvm into nodes when available / required / never.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-sandboxfleet}"
ENSURE_NESTED_VIRT="${ENSURE_NESTED_VIRT:-auto}"

command -v docker >/dev/null && command -v kind >/dev/null || {
	echo "need docker and kind" >&2
	exit 1
}

kvm_ok() { docker run --rm --device /dev/kvm busybox true >/dev/null 2>&1; }

WITH_KVM=0
case "${ENSURE_NESTED_VIRT}" in
auto) kvm_ok && WITH_KVM=1 ;;
1) kvm_ok || { echo "/dev/kvm required but not usable from Docker" >&2; exit 1; }; WITH_KVM=1 ;;
0) ;;
*) echo "ENSURE_NESTED_VIRT must be auto|1|0" >&2; exit 1 ;;
esac

mkdir -p "${ROOT}/bin"
CONFIG="${ROOT}/config/kind/cluster.yaml"
if [[ "${WITH_KVM}" == "1" ]]; then
	CONFIG="${ROOT}/bin/kind-config.yaml"
	cat >"${CONFIG}" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${CLUSTER_NAME}
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /dev/kvm
        containerPath: /dev/kvm
EOF
fi

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
	echo "Using existing kind cluster ${CLUSTER_NAME}"
	if [[ "${WITH_KVM}" == "1" ]]; then
		node="$(kind get nodes --name "${CLUSTER_NAME}" | head -n1)"
		docker exec "${node}" test -e /dev/kvm || {
			echo "cluster lacks /dev/kvm; run ./hack/cleanup-kind.sh and recreate" >&2
			exit 1
		}
	fi
else
	echo "Creating kind cluster ${CLUSTER_NAME} (kvm=${WITH_KVM})..."
	kind create cluster --name "${CLUSTER_NAME}" --config "${CONFIG}"
fi

if [[ "${WITH_KVM}" == "1" ]]; then
	for node in $(kind get nodes --name "${CLUSTER_NAME}"); do
		docker exec "${node}" chmod 666 /dev/kvm
	done
fi
