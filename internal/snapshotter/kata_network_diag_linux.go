//go:build linux

package snapshotter

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/AgentNaut/SandboxFleet/internal/runtime/kata/overlay"
)

// guestNetworkDebugCmd is run on the kata debug console when guest networking
// fails (substrate run.go network-config failure dump).
const guestNetworkDebugCmd = "ip addr 2>&1; echo '== route =='; ip route 2>&1; echo '== resolv =='; cat /etc/resolv.conf 2>&1"

// logKataGuestNetworkDiag dumps guest network state over the debug console and
// writes host bridge diagnostics when guest network setup fails.
func logKataGuestNetworkDiag(ctx context.Context, vsockPath, diagPath string, slotID int32, label string) {
	dump := overlay.DebugConsoleDump(ctx, vsockPath, guestNetworkDebugCmd)
	log.Printf("kata %s slot=%d guest network dump:\n%s", label, slotID, dump)
	if err := writeKataBridgeDiag(ctx, diagPath, slotID); err != nil {
		log.Printf("kata %s slot=%d write bridge diag: %v", label, slotID, err)
	}
}

// writeKataBridgeDiag records host-side sf-br0 state for e2e artifacts and CI
// worker logs (picked up by hack/collect-e2e-logs.sh network-*.txt patterns).
func writeKataBridgeDiag(ctx context.Context, path string, slotID int32) error {
	netCfg, err := guestNetForSlot(slotID)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "slot=%d guest_ip=%s/%s gw=%s gw_mac=%s\n",
		slotID, netCfg.IP, netCfg.Mask, netCfg.Gateway, netCfg.GatewayMAC)

	run := func(title string, args ...string) {
		fmt.Fprintf(&b, "\n=== %s: %s ===\n", title, strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		out, err := cmd.CombinedOutput()
		b.Write(out)
		if err != nil {
			fmt.Fprintf(&b, "\n[err] %v\n", err)
		}
	}
	run("ip_forward", "cat", "/proc/sys/net/ipv4/ip_forward")
	run("bridge_link", "ip", "link", "show", guestBridge)
	run("bridge_addr", "ip", "addr", "show", guestBridge)
	run("host_route", "ip", "route")
	run("iptables_forward", "iptables", "-S", "FORWARD")
	run("iptables_nat", "iptables", "-t", "nat", "-S", "POSTROUTING")

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	log.Printf("kata bridge diag: %s (%d bytes)", path, b.Len())
	return nil
}
