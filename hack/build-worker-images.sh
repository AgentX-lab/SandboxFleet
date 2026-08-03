#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASE_IMAGE="${BASE_IMAGE:-sandboxfleet-worker-base:latest}"
GVISOR_IMAGE="${GVISOR_IMAGE:-sandboxfleet-worker-gvisor:latest}"
KATA_IMAGE="${KATA_IMAGE:-sandboxfleet-worker-kata:latest}"
GVISOR_RELEASE="${GVISOR_RELEASE:-latest}"
KATA_VERSION="${KATA_VERSION:-4.0.0}"

usage() {
	echo "usage: $0 <runc|gvisor|kata|all>" >&2
	echo "  runc    build only the generic CRI Worker base image" >&2
	echo "  gvisor  build base, then the gVisor Worker image" >&2
	echo "  kata    build base, then the Kata (Cloud Hypervisor) Worker image" >&2
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

build_kata() {
	build_runc
	echo "Building Kata Worker image ${KATA_IMAGE} (from ${BASE_IMAGE}, kata ${KATA_VERSION})..."
	docker build \
		-f build/runtimes/kata/Dockerfile \
		--build-arg "BASE_IMAGE=${BASE_IMAGE}" \
		--build-arg "KATA_VERSION=${KATA_VERSION}" \
		-t "${KATA_IMAGE}" \
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
kata)
	build_kata
	echo "Done: ${BASE_IMAGE}"
	echo "Done: ${KATA_IMAGE}"
	;;
all)
	build_gvisor
	build_kata
	echo "Done: ${BASE_IMAGE}"
	echo "Done: ${GVISOR_IMAGE}"
	echo "Done: ${KATA_IMAGE}"
	;;
*)
	usage
	;;
esac
