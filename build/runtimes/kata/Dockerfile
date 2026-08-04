# syntax=docker/dockerfile:1.7

# Kata Worker image: generic CRI base + kata-containers (Cloud Hypervisor).
# Control-plane code stays runtime-agnostic; only this image knows about Kata.

ARG BASE_IMAGE=sandboxfleet-worker-base:latest
ARG KATA_VERSION=4.0.0

FROM ${BASE_IMAGE}
ARG TARGETARCH
ARG KATA_VERSION

# rsyslog: kata-shim reports failures via syslog; without /dev/log the real
# error is masked as "Unix syslog delivery error: unknown".
RUN apt-get update \
	&& DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		zstd \
		rsyslog \
	&& rm -rf /var/lib/apt/lists/*

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
	&& rm -rf /tmp/kata-static.tar.zst /tmp/kata-extract

COPY build/runtimes/kata/containerd.toml /etc/containerd/config.toml
