# syntax=docker/dockerfile:1.7

# gVisor Worker image: generic CRI base + runsc runtime handler.
# Control-plane code stays runtime-agnostic; only this image knows about gVisor.

ARG BASE_IMAGE=sandboxfleet-worker-base:latest
ARG GVISOR_RELEASE=latest

FROM ${BASE_IMAGE}
ARG TARGETARCH
ARG GVISOR_RELEASE

RUN case "${TARGETARCH}" in \
		amd64) ARCH=x86_64 ;; \
		arm64) ARCH=aarch64 ;; \
		*) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
	esac \
	&& curl -fsSL -o /usr/local/bin/runsc \
		"https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${ARCH}/runsc" \
	&& curl -fsSL -o /usr/local/bin/containerd-shim-runsc-v1 \
		"https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${ARCH}/containerd-shim-runsc-v1" \
	&& chmod +x /usr/local/bin/runsc /usr/local/bin/containerd-shim-runsc-v1

COPY build/runtimes/gvisor/containerd.toml /etc/containerd/config.toml
COPY build/runtimes/gvisor/runsc.toml /etc/containerd/runsc.toml
