# syntax=docker/dockerfile:1.7

# Kata Worker image: generic CRI base + kata-containers (Cloud Hypervisor).
# Control-plane code stays runtime-agnostic; only this image knows about Kata.
#
# Binary versions for snapshot/restore match substrate hack/microvm-assets/assemble.sh:
#   cloud-hypervisor v52.0 + virtiofsd v1.14.0 (kata-static still ships virtiofsd
#   1.13.3 whose old vhost deadlocks CH restore on virtio-fs).

ARG BASE_IMAGE=sandboxfleet-worker-base:latest
ARG KATA_VERSION=4.0.0
ARG CH_VERSION=v52.0
ARG VIRTIOFSD_VERSION=v1.14.0

FROM ${BASE_IMAGE}
ARG TARGETARCH
ARG KATA_VERSION
ARG CH_VERSION
ARG VIRTIOFSD_VERSION

# rsyslog: kata-shim reports failures via syslog; without /dev/log the real
# error is masked as "Unix syslog delivery error: unknown".
# unzip: extract upstream virtiofsd amd64 prebuilt (same zip as substrate).
RUN apt-get update \
	&& DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		zstd \
		rsyslog \
		unzip \
	&& rm -rf /var/lib/apt/lists/*

# Cap guest RAM: default_memory is the boot size; default_maxmemory caps CLH
# hotplug_size. Leaving maxmemory at 0 uses host RAM (~16Gi on CI) and hangs
# Cloud Hypervisor restore inside a 4Gi Worker.
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
	&& ln -sf /opt/kata/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-v2 \
	&& ln -sf /opt/kata/bin/kata-runtime /usr/local/bin/kata-runtime \
	&& test -x /opt/kata/bin/cloud-hypervisor \
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

# Overlay CH + virtiofsd (substrate assemble.sh versions). Parent CRI VMs and
# SandboxFleet restore both resolve these paths under /opt/kata.
RUN set -eux; \
	case "${TARGETARCH}" in \
		amd64) CH_ASSET=cloud-hypervisor-static ;; \
		arm64) CH_ASSET=cloud-hypervisor-static-aarch64 ;; \
		*) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
	esac; \
	curl -fsSL -o /opt/kata/bin/cloud-hypervisor \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/${CH_ASSET}"; \
	chmod +x /opt/kata/bin/cloud-hypervisor; \
	/opt/kata/bin/cloud-hypervisor --version; \
	VIRTIOFSD_DST=/opt/kata/libexec/virtiofsd; \
	mkdir -p /opt/kata/libexec; \
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
	ln -sfn "${VIRTIOFSD_DST}" /opt/kata/bin/virtiofsd; \
	"${VIRTIOFSD_DST}" --version

COPY build/runtimes/kata/containerd.toml /etc/containerd/config.toml
