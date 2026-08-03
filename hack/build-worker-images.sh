#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASE_IMAGE="${BASE_IMAGE:-sandboxfleet-worker-base:latest}"
GVISOR_IMAGE="${GVISOR_IMAGE:-sandboxfleet-worker-gvisor:latest}"
GVISOR_RELEASE="${GVISOR_RELEASE:-latest}"

usage() {
	echo "usage: $0 <runc|gvisor|all>" >&2
	echo "  runc    build only the generic CRI Worker base image" >&2
	echo "  gvisor  build base, then the gVisor Worker image" >&2
	echo "  all     build every Worker runtime variant" >&2
	exit 1
}

build_runc() {
	echo "Building Worker base image ${BASE_IMAGE}..."
	docker build \
		-f build/worker/Dockerfile \
		-t "${BASE_IMAGE}" \
		"${ROOT}"
}

build_gvisor() {
	# gVisor image FROM the base image; ensure base exists for this build.
	build_runc
	echo "Building gVisor Worker image ${GVISOR_IMAGE} (from ${BASE_IMAGE})..."
	docker build \
		-f build/runtimes/gvisor/Dockerfile \
		--build-arg "BASE_IMAGE=${BASE_IMAGE}" \
		--build-arg "GVISOR_RELEASE=${GVISOR_RELEASE}" \
		-t "${GVISOR_IMAGE}" \
		"${ROOT}"
}

TARGET="${1:-}"
[[ -n "${TARGET}" ]] || usage

cd "${ROOT}"

case "${TARGET}" in
runc)
	build_runc
	echo "Done: ${BASE_IMAGE}"
	;;
gvisor)
	build_gvisor
	echo "Done: ${BASE_IMAGE}"
	echo "Done: ${GVISOR_IMAGE}"
	;;
all)
	build_gvisor
	echo "Done: ${BASE_IMAGE}"
	echo "Done: ${GVISOR_IMAGE}"
	;;
*)
	usage
	;;
esac
