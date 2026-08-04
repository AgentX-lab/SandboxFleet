#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-sandboxfleet}"
E2E_KUBE_CONTEXT="${E2E_KUBE_CONTEXT:-kind-${CLUSTER_NAME}}"
E2E_TIMEOUT="${E2E_TIMEOUT:-10m}"
KUBECONFIG="${KUBECONFIG:-${ROOT}/bin/KUBECONFIG}"

need() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "missing required command: $1" >&2
		exit 1
	}
}

need kubectl
need go

cd "${ROOT}"

if [[ ! -f "${KUBECONFIG}" ]]; then
	echo "missing kubeconfig at ${KUBECONFIG}" >&2
	echo "run ./hack/deploy-kind.sh first" >&2
	exit 1
fi

if ! kubectl --kubeconfig "${KUBECONFIG}" --context "${E2E_KUBE_CONTEXT}" get ns sandboxfleet-system >/dev/null 2>&1; then
	echo "SandboxFleet is not installed in context ${E2E_KUBE_CONTEXT}" >&2
	echo "run ./hack/deploy-kind.sh first" >&2
	exit 1
fi

if [[ -f "${ROOT}/bin/runtime.env" ]]; then
	# shellcheck disable=SC1091
	source "${ROOT}/bin/runtime.env"
fi

export KUBECONFIG
export E2E_KUBE_CONTEXT
export E2E_RUNTIME_HANDLER="${E2E_RUNTIME_HANDLER:-runsc}"

echo "Running e2e against kubeconfig=${KUBECONFIG} context=${E2E_KUBE_CONTEXT} handler=${E2E_RUNTIME_HANDLER}..."
# -failfast: first failure (e.g. lifecycle timeout) aborts the rest of the suite.
go test ./test/e2e/ -tags=e2e -count=1 -v -failfast -timeout="${E2E_TIMEOUT}"

echo "E2E tests passed."
