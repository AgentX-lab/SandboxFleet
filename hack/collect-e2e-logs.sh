#!/usr/bin/env bash
# Collect cluster diagnostics after an e2e failure.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-sandboxfleet}"
E2E_KUBE_CONTEXT="${E2E_KUBE_CONTEXT:-kind-${CLUSTER_NAME}}"
KUBECONFIG="${KUBECONFIG:-${ROOT}/bin/KUBECONFIG}"
OUT="${E2E_LOG_DIR:-${ROOT}/bin/e2e-logs}"

if [[ ! -f "${KUBECONFIG}" ]]; then
	echo "no kubeconfig at ${KUBECONFIG}; skip log collection" >&2
	exit 0
fi

mkdir -p "${OUT}"
export KUBECONFIG
kc=(kubectl --kubeconfig "${KUBECONFIG}" --context "${E2E_KUBE_CONTEXT}")

echo "Collecting e2e diagnostics into ${OUT}..."

{
	echo "=== context ==="
	"${kc[@]}" config current-context || true
	echo "=== nodes ==="
	"${kc[@]}" get nodes -o wide || true
	echo "=== pods -A ==="
	"${kc[@]}" get pods -A -o wide || true
	echo "=== sandboxpools -A ==="
	"${kc[@]}" get sandboxpools -A -o wide || true
	echo "=== sandboxes -A ==="
	"${kc[@]}" get sandboxes -A -o wide || true
} >"${OUT}/summary.txt" 2>&1 || true

"${kc[@]}" get sandboxpools -A -o yaml >"${OUT}/sandboxpools.yaml" 2>&1 || true
"${kc[@]}" get sandboxes -A -o yaml >"${OUT}/sandboxes.yaml" 2>&1 || true
"${kc[@]}" get pods -A -o yaml >"${OUT}/pods.yaml" 2>&1 || true
"${kc[@]}" get events -A --sort-by=.lastTimestamp >"${OUT}/events.txt" 2>&1 || true

"${kc[@]}" -n sandboxfleet-system logs deploy/sandboxfleet-controller --all-containers --tail=500 \
	>"${OUT}/controller.log" 2>&1 || true

# Worker pods (managed by SandboxFleet).
while read -r ns name; do
	[[ -z "${ns:-}" || -z "${name:-}" ]] && continue
	safe="${ns}_${name}"
	"${kc[@]}" -n "${ns}" logs "${name}" --all-containers --tail=1000 \
		>"${OUT}/worker-${safe}.log" 2>&1 || true
	# CrashLoop overwrites current logs; keep the previous container's stdout/stderr.
	"${kc[@]}" -n "${ns}" logs "${name}" --all-containers --previous --tail=1000 \
		>"${OUT}/worker-${safe}.previous.log" 2>&1 || true
	"${kc[@]}" -n "${ns}" describe pod "${name}" \
		>"${OUT}/worker-${safe}.describe.txt" 2>&1 || true
	# Kata restore dumps CH/virtiofsd logs under the worker state dir.
	# Pull small logs first (avoid losing them when the full state tar truncates on
	# multi-GiB memory-ranges during artifact upload).
	"${kc[@]}" -n "${ns}" exec "${name}" -- sh -c '
		for f in /var/lib/sandboxfleet/kata/*/cloud-hypervisor.log \
			/var/lib/sandboxfleet/kata/*/cloud-hypervisor.stderr.log \
			/var/lib/sandboxfleet/kata/*/virtiofsd-*.log; do
			[ -f "$f" ] || continue
			echo "===== $f ====="
			# Cap each file so the artifact stays small.
			tail -c 256K "$f" 2>/dev/null || true
			echo
		done
	' >"${OUT}/kata-ch-logs-${safe}.txt" 2>&1 || true
	"${kc[@]}" -n "${ns}" exec "${name}" -- sh -c \
		'tar -C /var/lib/sandboxfleet/kata --exclude="*/memory-ranges" --exclude="*/rootfs-share-*.tar" -cf - . 2>/dev/null || true' \
		>"${OUT}/kata-state-${safe}.tar" 2>/dev/null || true
	# gVisor restore roots (runsc state) for create/restore hang diagnosis.
	"${kc[@]}" -n "${ns}" exec "${name}" -- sh -c \
		'tar -C /var/lib/sandboxfleet/runsc -cf - . 2>/dev/null || true' \
		>"${OUT}/runsc-state-${safe}.tar" 2>/dev/null || true
	# Guest-bridge / netns / iptables dump for restore egress/DNS failures.
	"${kc[@]}" -n "${ns}" exec "${name}" -- sh -c '
		{
			echo "=== ip_forward ==="
			cat /proc/sys/net/ipv4/ip_forward 2>&1 || true
			echo "=== host resolv.conf ==="
			cat /etc/resolv.conf 2>&1 || true
			echo "=== ip addr ==="
			ip addr 2>&1 || true
			echo "=== ip route ==="
			ip route 2>&1 || true
			echo "=== sf-br0 ==="
			ip link show sf-br0 2>&1 || true
			echo "=== netns list ==="
			ip netns list 2>&1 || true
			for ns in $(ip netns list 2>/dev/null | awk "{print \$1}"); do
				echo "=== netns ${ns} addr ==="
				ip netns exec "${ns}" ip addr 2>&1 || true
				echo "=== netns ${ns} route ==="
				ip netns exec "${ns}" ip route 2>&1 || true
				echo "=== netns ${ns} ping gw 10.89.0.1 ==="
				ip netns exec "${ns}" ping -c1 -W2 10.89.0.1 2>&1 || true
				echo "=== netns ${ns} ping 8.8.8.8 ==="
				ip netns exec "${ns}" ping -c1 -W2 8.8.8.8 2>&1 || true
				dns=$(awk "/^nameserver/{print \$2; exit}" /etc/resolv.conf 2>/dev/null || true)
				if [ -n "${dns}" ]; then
					echo "=== netns ${ns} ping nameserver ${dns} ==="
					ip netns exec "${ns}" ping -c1 -W2 "${dns}" 2>&1 || true
				fi
			done
			echo "=== iptables -S FORWARD ==="
			iptables -S FORWARD 2>&1 || true
			echo "=== iptables -t nat -S POSTROUTING ==="
			iptables -t nat -S POSTROUTING 2>&1 || true
			echo "=== *.net.diag.txt ==="
			ls -la /var/lib/sandboxfleet/runsc/*.net.diag.txt 2>&1 || true
			for f in /var/lib/sandboxfleet/runsc/*.net.diag.txt; do
				[ -f "$f" ] || continue
				echo "----- $f -----"
				cat "$f" 2>&1 || true
			done
		} 2>&1
	' >"${OUT}/network-${safe}.txt" 2>&1 || true
done < <("${kc[@]}" get pods -A -l sandboxfleet.io/managed=true -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

# Describe each Sandbox.
while read -r ns name; do
	[[ -z "${ns:-}" || -z "${name:-}" ]] && continue
	safe="${ns}_${name}"
	"${kc[@]}" -n "${ns}" describe sandbox "${name}" \
		>"${OUT}/sandbox-${safe}.describe.txt" 2>&1 || true
done < <("${kc[@]}" get sandboxes -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

echo "Wrote diagnostics under ${OUT}"
ls -la "${OUT}" || true
