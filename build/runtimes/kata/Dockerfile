# syntax=docker/dockerfile:1.7

# Kata Worker image: generic base + the kata guest assets (Cloud Hypervisor).
# Control-plane code stays runtime-agnostic; only this image knows about Kata.
#
# The Worker boots and owns cloud-hypervisor itself (no kata shim, no CRI pod
# sandbox), so kata-static is here only for its guest assets: the kernel, the
# guest OS image and configuration-clh.toml (guest sizing + kernel_params).
# The VMM binaries come from /opt/sandboxfleet and match substrate
# hack/microvm-assets/assemble.sh: CH v52 + virtiofsd 1.14, the pair that speaks
# --migration-mode=find-paths, so cold boot and restore share one migratable
# virtio-fs stack. containerd still runs on these Workers, but only to unpack
# images into the guest rootfs lower.

ARG BASE_IMAGE=sandboxfleet-worker-base:latest
ARG KATA_VERSION=4.0.0
ARG CH_VERSION=v52.0
ARG VIRTIOFSD_VERSION=v1.14.0

FROM ${BASE_IMAGE}
ARG TARGETARCH
ARG KATA_VERSION
ARG CH_VERSION
ARG VIRTIOFSD_VERSION

# unzip: extract upstream virtiofsd amd64 prebuilt (same zip as substrate).
# rsyslog keeps /dev/log present for anything in the base image that logs there.
RUN apt-get update \
	&& DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		zstd \
		rsyslog \
		unzip \
	&& rm -rf /var/lib/apt/lists/*

# Cap guest RAM: the Worker reads default_memory / default_vcpus / kernel_params
# out of this config when it builds the VmConfig, so 512MiB keeps a booted guest
# inside the 4Gi CI Worker. default_maxmemory bounds CLH hotplug for any tooling
# that still reads the file.
RUN case "${TARGETARCH}" in \
		amd64) ARCH=amd64 ;; \
		arm64) ARCH=arm64 ;; \
		*) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
	esac \
	&& curl -fsSL -o /tmp/kata-static.tar.zst \
		"https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/kata-static-${KATA_VERSION}-${ARCH}.tar.zst" \
	&& mkdir -p /tmp/kata-extract \
	&& tar --zstd -xf /tmp/kata-static.tar.zst -C /tmp/kata-extract \
	&& cp -a /tmp/kata-extract/opt/kata /opt/kata \
	&& test -f /opt/kata/share/kata-containers/vmlinux.container \
	&& test -f /opt/kata/share/kata-containers/kata-containers.img \
	&& test -f /opt/kata/share/defaults/kata-containers/configuration-clh.toml \
	&& sed -i -E 's/^default_memory[[:space:]]*=[[:space:]]*[0-9]+/default_memory = 512/' \
		/opt/kata/share/defaults/kata-containers/configuration-clh.toml \
	&& grep -E '^default_memory[[:space:]]*=[[:space:]]*512$' \
		/opt/kata/share/defaults/kata-containers/configuration-clh.toml \
	&& if grep -Eq '^[[:space:]]*default_maxmemory[[:space:]]*=' \
		/opt/kata/share/defaults/kata-containers/configuration-clh.toml; then \
		sed -i -E 's/^[[:space:]]*default_maxmemory[[:space:]]*=[[:space:]]*[0-9]+/default_maxmemory = 2048/' \
			/opt/kata/share/defaults/kata-containers/configuration-clh.toml; \
	else \
		printf '\ndefault_maxmemory = 2048\n' \
			>> /opt/kata/share/defaults/kata-containers/configuration-clh.toml; \
	fi \
	&& grep -E '^default_maxmemory[[:space:]]*=[[:space:]]*2048$' \
		/opt/kata/share/defaults/kata-containers/configuration-clh.toml \
	&& rm -rf /tmp/kata-static.tar.zst /tmp/kata-extract

# Install CH v52 + virtiofsd 1.14 under /opt/sandboxfleet. The Worker launches
# both directly (see SANDBOXFLEET_* env below); the kata config is not patched to
# point at them because no shim starts a VM here.
RUN set -eux; \
	case "${TARGETARCH}" in \
		amd64) CH_ASSET=cloud-hypervisor-static ;; \
		arm64) CH_ASSET=cloud-hypervisor-static-aarch64 ;; \
		*) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
	esac; \
	mkdir -p /opt/sandboxfleet/bin; \
	curl -fsSL -o /opt/sandboxfleet/bin/cloud-hypervisor \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/${CH_ASSET}"; \
	chmod +x /opt/sandboxfleet/bin/cloud-hypervisor; \
	/opt/sandboxfleet/bin/cloud-hypervisor --version; \
	VIRTIOFSD_DST=/opt/sandboxfleet/bin/virtiofsd; \
	if [ "${TARGETARCH}" = "amd64" ]; then \
		# Same upload URL + sha as substrate hack/microvm-assets/assemble.sh.
		VIRTIOFSD_ZIP_URL="https://gitlab.com/-/project/21523468/uploads/f505704014ae7a816e515f2a05a93d8b/virtiofsd-v1.14.0.zip"; \
		VIRTIOFSD_ZIP_SHA256="2e4fe9571f492b00baa34bc4e708e950039c5da05b830b31a8d179cb6ac8978e"; \
		curl -fsSL -o /tmp/virtiofsd.zip "${VIRTIOFSD_ZIP_URL}"; \
		echo "${VIRTIOFSD_ZIP_SHA256}  /tmp/virtiofsd.zip" | sha256sum -c -; \
		mkdir -p /tmp/virtiofsd-extract; \
		unzip -q -o /tmp/virtiofsd.zip -d /tmp/virtiofsd-extract; \
		cp /tmp/virtiofsd-extract/target/x86_64-unknown-linux-musl/release/virtiofsd "${VIRTIOFSD_DST}"; \
		rm -rf /tmp/virtiofsd.zip /tmp/virtiofsd-extract; \
	else \
		DEBIAN_FRONTEND=noninteractive apt-get update; \
		DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
			ca-certificates git build-essential pkg-config libcap-ng-dev libseccomp-dev curl; \
		curl -fsSL https://sh.rustup.rs | sh -s -- -y --default-toolchain stable; \
		. "$HOME/.cargo/env"; \
		git clone --depth 1 --branch "${VIRTIOFSD_VERSION}" https://gitlab.com/virtio-fs/virtiofsd.git /tmp/virtiofsd-src; \
		( cd /tmp/virtiofsd-src && grep -E '^(vhost|vhost-user-backend) =' Cargo.toml && cargo build --release ); \
		cp /tmp/virtiofsd-src/target/release/virtiofsd "${VIRTIOFSD_DST}"; \
		rm -rf /tmp/virtiofsd-src "$HOME/.cargo" "$HOME/.rustup"; \
		apt-get purge -y git build-essential pkg-config libcap-ng-dev libseccomp-dev; \
		apt-get autoremove -y; \
		rm -rf /var/lib/apt/lists/*; \
	fi; \
	chmod +x "${VIRTIOFSD_DST}"; \
	"${VIRTIOFSD_DST}" --version

# Cold boot and restore both launch these directly.
ENV SANDBOXFLEET_CLOUD_HYPERVISOR_PATH=/opt/sandboxfleet/bin/cloud-hypervisor \
	SANDBOXFLEET_VIRTIOFSD_PATH=/opt/sandboxfleet/bin/virtiofsd

COPY build/runtimes/kata/containerd.toml /etc/containerd/config.toml
