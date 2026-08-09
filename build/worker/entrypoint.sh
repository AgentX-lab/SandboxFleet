#!/bin/sh
set -eu

mkdir -p /var/lib/containerd /run/containerd /opt/cni/bin /etc/cni/net.d

# Optional: present only on the Kata Worker image. Lets kata-shim log real errors.
if command -v rsyslogd >/dev/null 2>&1; then
	mkdir -p /var/run
	rsyslogd
fi

# Nested sandboxes need forwarding + NAT through the Worker Pod network.
if [ -w /proc/sys/net/ipv4/ip_forward ]; then
	echo 1 >/proc/sys/net/ipv4/ip_forward
fi
# Restore guests use sf-br0 on 10.89.0.0/16 (not CNI's 10.88.0.0/16 on cni0).
# FORWARD ACCEPT is required on kind/docker (FORWARD often DROP) so sf-br0 guests
# can reach the internet — same rules ensureOutboundNAT installs.
if command -v iptables >/dev/null 2>&1; then
	iptables -t nat -C POSTROUTING -s 10.89.0.0/16 ! -o sf-br0 -j MASQUERADE 2>/dev/null \
		|| iptables -t nat -A POSTROUTING -s 10.89.0.0/16 ! -o sf-br0 -j MASQUERADE
	iptables -C FORWARD -i sf-br0 -j ACCEPT 2>/dev/null \
		|| iptables -I FORWARD -i sf-br0 -j ACCEPT
	iptables -C FORWARD -o sf-br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
		|| iptables -I FORWARD -o sf-br0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
fi

containerd --config /etc/containerd/config.toml &
containerd_pid=$!

cleanup() {
	if [ -n "${worker_pid:-}" ]; then
		kill "${worker_pid}" 2>/dev/null || true
		wait "${worker_pid}" 2>/dev/null || true
	fi
	kill "${containerd_pid}" 2>/dev/null || true
	wait "${containerd_pid}" 2>/dev/null || true
}
trap cleanup INT TERM EXIT

i=0
while [ ! -S /run/containerd/containerd.sock ]; do
	i=$((i + 1))
	if [ "${i}" -gt 100 ]; then
		echo "timed out waiting for containerd socket" >&2
		exit 1
	fi
	if ! kill -0 "${containerd_pid}" 2>/dev/null; then
		echo "containerd exited before becoming ready" >&2
		wait "${containerd_pid}" || true
		exit 1
	fi
	sleep 0.1
done

/usr/local/bin/sandboxfleet-worker "$@" &
worker_pid=$!
wait "${worker_pid}"
status=$?
trap - INT TERM EXIT
cleanup
exit "${status}"
