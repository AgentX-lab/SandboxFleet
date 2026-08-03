# syntax=docker/dockerfile:1.7

# Generic CRI Worker base: agent + local containerd + CNI.
# Runtime-specific binaries (gVisor, Kata, ...) belong in build/runtimes/<name>/.

ARG GO_VERSION=1.26.2
ARG CONTAINERD_VERSION=1.7.27
ARG RUNC_VERSION=1.2.6
ARG CNI_PLUGINS_VERSION=1.6.2

FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/sandboxfleet-worker ./cmd/worker

FROM ubuntu:24.04 AS base
ARG TARGETARCH
ARG CONTAINERD_VERSION
ARG RUNC_VERSION
ARG CNI_PLUGINS_VERSION

RUN apt-get update \
	&& DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		ca-certificates \
		curl \
		iptables \
		iproute2 \
		pigz \
		xz-utils \
	&& rm -rf /var/lib/apt/lists/*

RUN curl -fsSL "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz" \
	| tar -xz -C /usr/local \
	&& curl -fsSL -o /usr/local/sbin/runc "https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${TARGETARCH}" \
	&& chmod +x /usr/local/sbin/runc \
	&& mkdir -p /opt/cni/bin /etc/cni/net.d \
	&& curl -fsSL "https://github.com/containernetworking/plugins/releases/download/v${CNI_PLUGINS_VERSION}/cni-plugins-linux-${TARGETARCH}-v${CNI_PLUGINS_VERSION}.tgz" \
	| tar -xz -C /opt/cni/bin

COPY --from=build /out/sandboxfleet-worker /usr/local/bin/sandboxfleet-worker
COPY build/worker/containerd.toml /etc/containerd/config.toml
COPY build/worker/cni/10-sandboxfleet.conflist /etc/cni/net.d/10-sandboxfleet.conflist
COPY build/worker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/sandboxfleet-worker

# Worker flags (name/namespace/pool/slots/runtime-handler/...) are supplied by the controller.
ENTRYPOINT ["/entrypoint.sh"]
