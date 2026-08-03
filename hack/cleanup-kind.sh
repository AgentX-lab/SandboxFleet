#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-sandboxfleet}"

if ! command -v kind >/dev/null 2>&1; then
	echo "missing required command: kind" >&2
	exit 1
fi

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
	echo "Deleting kind cluster ${CLUSTER_NAME}..."
	kind delete cluster --name "${CLUSTER_NAME}"
else
	echo "kind cluster ${CLUSTER_NAME} does not exist"
fi
