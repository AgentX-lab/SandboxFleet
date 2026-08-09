//go:build linux

package snapshotter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"golang.org/x/sys/unix"
)

// restoreNetInfo records the netns/veth created for a restored gVisor child.
type restoreNetInfo struct {
	Netns  string `json:"netns"`
	Veth   string `json:"veth"`
	SlotID int32  `json:"slotID"`
	IP     string `json:"ip"`
}

func (g *GVisor) restoreNetInfoPath(name string) string {
	return filepath.Join(g.RestoreRoot, name+".net.json")
}

func (g *GVisor) saveRestoreNetInfo(name string, info restoreNetInfo) error {
	if err := os.MkdirAll(g.RestoreRoot, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(g.restoreNetInfoPath(name), raw, 0o600)
}

func (g *GVisor) loadRestoreNetInfo(name string) (restoreNetInfo, error) {
	raw, err := os.ReadFile(g.restoreNetInfoPath(name))
	if err != nil {
		return restoreNetInfo{}, err
	}
	var info restoreNetInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return restoreNetInfo{}, err
	}
	return info, nil
}

// createRestoreNetwork creates sf-br0 + netns + veth with a unique 10.89.0.x address.
// runsc restore then runs with --network=host inside that netns (host == the netns).
func (g *GVisor) createRestoreNetwork(ctx context.Context, slotID int32, name string) (restoreNetInfo, error) {
	netCfg, err := guestNetForSlot(slotID)
	if err != nil {
		return restoreNetInfo{}, err
	}
	if err := ensureSharedBridge(ctx); err != nil {
		return restoreNetInfo{}, err
	}
	ensureOutboundNAT(ctx)

	netns := fmt.Sprintf("sfg%d", slotID)
	veth := fmt.Sprintf("sfv%d", slotID)
	_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})

	if err := runIPCommand(ctx, "netns", "add", netns); err != nil {
		return restoreNetInfo{}, fmt.Errorf("netns add: %w", err)
	}
	peer := veth + "p"
	if err := runIPCommand(ctx, "link", "add", veth, "type", "veth", "peer", "name", peer); err != nil {
		_ = runIPCommand(ctx, "netns", "del", netns)
		return restoreNetInfo{}, fmt.Errorf("veth add: %w", err)
	}
	if err := runIPCommand(ctx, "link", "set", peer, "netns", netns); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, fmt.Errorf("veth set netns: %w", err)
	}
	if err := runIPCommand(ctx, "link", "set", veth, "master", guestBridge); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, fmt.Errorf("veth master bridge: %w", err)
	}
	if err := runIPCommand(ctx, "link", "set", veth, "up"); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, err
	}

	ns := func(args ...string) error {
		return runIPCommand(ctx, append([]string{"netns", "exec", netns, "ip"}, args...)...)
	}
	if err := ns("link", "set", "lo", "up"); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, err
	}
	if err := ns("link", "set", peer, "name", netCfg.Iface); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, err
	}
	if err := ns("link", "set", netCfg.Iface, "address", netCfg.MAC); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, err
	}
	if err := ns("addr", "add", netCfg.IP+"/"+netCfg.Mask, "dev", netCfg.Iface); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, err
	}
	if err := ns("link", "set", netCfg.Iface, "up"); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, err
	}
	if err := ns("route", "add", "default", "via", netCfg.Gateway); err != nil {
		_ = deleteRestoreNetwork(ctx, restoreNetInfo{Netns: netns, Veth: veth})
		return restoreNetInfo{}, err
	}

	info := restoreNetInfo{Netns: netns, Veth: veth, SlotID: slotID, IP: netCfg.IP}
	if err := g.saveRestoreNetInfo(name, info); err != nil {
		_ = deleteRestoreNetwork(ctx, info)
		return restoreNetInfo{}, err
	}
	g.writeRestoreNetworkDiag(ctx, name, info, netCfg)
	return info, nil
}

func deleteRestoreNetwork(ctx context.Context, info restoreNetInfo) error {
	var first error
	if info.Veth != "" {
		if err := runIPCommand(ctx, "link", "del", info.Veth); err != nil && first == nil {
			// already gone is fine
			if !strings.Contains(err.Error(), "Cannot find device") && !strings.Contains(err.Error(), "does not exist") {
				first = err
			}
		}
	}
	if info.Netns != "" {
		if err := runIPCommand(ctx, "netns", "del", info.Netns); err != nil && first == nil {
			if !strings.Contains(err.Error(), "No such file") && !strings.Contains(err.Error(), "does not exist") {
				first = err
			}
		}
	}
	return first
}

// runInNetworkNamespace runs runsc inside a named netns while keeping the
// Worker's mount namespace (and its cgroup2 view).
//
// Do NOT use `ip netns exec`: it creates a new mount ns and remounts /sys,
// undoing SetupCgroupDelegation so runsc IsOnlyV2() fails and probes
// /sys/fs/cgroup/memory. Matches substrate ateomnet.NetNSDo (setns NET only).
//
// Stdout/Stderr must be *os.File (or nil). Buffers/MultiWriter make Go use a
// pipe; runsc create's gofer/sandbox inherit the write end and cmd.Wait hangs
// forever waiting for EOF (gvisor#12198 / #4544).
func (g *GVisor) runInNetworkNamespace(ctx context.Context, nsName string, args []string, logPath string) error {
	return doInNamedNetNS(nsName, func() error {
		cmd := exec.CommandContext(ctx, g.RunscPath, args...)
		var logFile *os.File
		if logPath != "" {
			f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("open runsc log %q: %w", logPath, err)
			}
			logFile = f
			cmd.Stdout = f
			cmd.Stderr = f
			defer f.Close()
		}
		if err := cmd.Run(); err != nil {
			msg := ""
			if logFile != nil {
				_, _ = logFile.Seek(0, 0)
				if b, rerr := io.ReadAll(logFile); rerr == nil {
					msg = strings.TrimSpace(string(b))
				}
			}
			if msg == "" {
				return fmt.Errorf("%s %s: %w", g.RunscPath, strings.Join(args, " "), err)
			}
			return fmt.Errorf("%s %s: %w: %s", g.RunscPath, strings.Join(args, " "), err, msg)
		}
		return nil
	})
}

// execInNetworkNamespace runs `runsc exec` inside the restore netns.
// Stdout/stderr use *os.File (not pipes) — same constraint as create/restore.
func (g *GVisor) execInNetworkNamespace(ctx context.Context, nsName string, args []string) (sandboxruntime.ExecResult, error) {
	outFile, err := os.CreateTemp("", "sandboxfleet-runsc-exec-out-*")
	if err != nil {
		return sandboxruntime.ExecResult{}, err
	}
	outPath := outFile.Name()
	defer func() { _ = os.Remove(outPath) }()

	errFile, err := os.CreateTemp("", "sandboxfleet-runsc-exec-err-*")
	if err != nil {
		_ = outFile.Close()
		return sandboxruntime.ExecResult{}, err
	}
	errPath := errFile.Name()
	defer func() { _ = os.Remove(errPath) }()

	runErr := doInNamedNetNS(nsName, func() error {
		cmd := exec.CommandContext(ctx, g.RunscPath, args...)
		cmd.Stdout = outFile
		cmd.Stderr = errFile
		return cmd.Run()
	})
	_ = outFile.Close()
	_ = errFile.Close()

	stdout, _ := os.ReadFile(outPath)
	stderr, _ := os.ReadFile(errPath)
	result := sandboxruntime.ExecResult{
		Stdout: string(stdout),
		Stderr: string(stderr),
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = int32(exitErr.ExitCode())
			return result, nil
		}
		return result, fmt.Errorf("runsc exec: %w: %s", runErr, strings.TrimSpace(string(stderr)))
	}
	return result, nil
}

// doInNamedNetNS switches the current OS thread into /var/run/netns/<name>
// (CLONE_NEWNET only), runs fn, then restores the previous netns.
func doInNamedNetNS(nsName string, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	target, err := os.Open(filepath.Join("/var/run/netns", nsName))
	if err != nil {
		return fmt.Errorf("open netns %q: %w", nsName, err)
	}
	defer target.Close()

	origin, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("open current netns: %w", err)
	}
	defer origin.Close()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("setns net %q: %w", nsName, err)
	}
	defer func() {
		if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
			// Same as substrate: better to crash than leave a thread in the wrong netns.
			panic(fmt.Sprintf("failed to restore original netns: %v", err))
		}
	}()
	return fn()
}

func ensureSharedBridge(ctx context.Context) error {
	if err := runIPCommand(ctx, "link", "show", guestBridge); err != nil {
		if err := runIPCommand(ctx, "link", "add", guestBridge, "type", "bridge"); err != nil {
			return fmt.Errorf("create bridge %s: %w", guestBridge, err)
		}
	}
	_ = runIPCommand(ctx, "addr", "replace", guestGateway+"/"+guestMask, "dev", guestBridge)
	if err := runIPCommand(ctx, "link", "set", guestBridge, "up"); err != nil {
		return fmt.Errorf("bridge up: %w", err)
	}
	return nil
}

func ensureOutboundNAT(ctx context.Context) {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		log.Printf("guest egress: enable ip_forward: %v", err)
	} else {
		log.Printf("guest egress: ip_forward=1")
	}

	type rule struct {
		append bool
		args   []string
	}
	rules := []rule{
		// Match build/worker/entrypoint.sh: MASQUERADE guest subnet out of the pod
		// (skip hairpin onto sf-br0). guestSubnet is 10.89/16, distinct from cni0.
		{true, []string{"-t", "nat", "-C", "POSTROUTING", "-s", guestSubnet, "!", "-o", guestBridge, "-j", "MASQUERADE"}},
		// kind/docker often leave FORWARD at DROP; without these, sf-br0 guests
		// cannot reach the internet (DNS fails with -3).
		{false, []string{"-C", "FORWARD", "-i", guestBridge, "-j", "ACCEPT"}},
		{false, []string{"-C", "FORWARD", "-o", guestBridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
	}
	for _, r := range rules {
		if err := ensureIptablesRule(ctx, r.append, r.args...); err != nil {
			log.Printf("guest egress: iptables FAILED %v: %v", r.args, err)
		}
	}
}

// ensureIptablesRule runs iptables with checkArgs (must include -C). If the
// rule is missing, rewrites -C to -A (when appendRule) or -I (when !appendRule,
// used for FORWARD so we precede docker/kind DROP policies) and applies it.
func ensureIptablesRule(ctx context.Context, appendRule bool, checkArgs ...string) error {
	check := exec.CommandContext(ctx, "iptables", checkArgs...)
	if _, err := check.CombinedOutput(); err == nil {
		log.Printf("guest egress: iptables present %v", checkArgs)
		return nil
	}
	addArgs := append([]string(nil), checkArgs...)
	for i, a := range addArgs {
		if a != "-C" {
			continue
		}
		if appendRule {
			addArgs[i] = "-A"
		} else {
			addArgs[i] = "-I"
		}
		break
	}
	out, err := exec.CommandContext(ctx, "iptables", addArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w: %s", addArgs, err, strings.TrimSpace(string(out)))
	}
	log.Printf("guest egress: iptables installed %v", addArgs)
	return nil
}

// writeRestoreNetworkDiag captures host + netns networking state for e2e
// artifacts (picked up via runsc-state-*.tar and collect-e2e-logs.sh).
func (g *GVisor) writeRestoreNetworkDiag(ctx context.Context, name string, info restoreNetInfo, netCfg guestNet) {
	var b strings.Builder
	fmt.Fprintf(&b, "name=%s netns=%s veth=%s slot=%d ip=%s/%s gw=%s\n",
		name, info.Netns, info.Veth, info.SlotID, netCfg.IP, netCfg.Mask, netCfg.Gateway)

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
	run("host_resolv", "cat", "/etc/resolv.conf")
	run("iptables_forward", "iptables", "-S", "FORWARD")
	run("iptables_nat", "iptables", "-t", "nat", "-S", "POSTROUTING")
	run("bridge", "ip", "link", "show", guestBridge)
	run("host_route", "ip", "route")
	run("netns_addr", "ip", "netns", "exec", info.Netns, "ip", "addr")
	run("netns_route", "ip", "netns", "exec", info.Netns, "ip", "route")
	run("ping_gw", "ip", "netns", "exec", info.Netns, "ping", "-c1", "-W2", netCfg.Gateway)
	run("ping_8888", "ip", "netns", "exec", info.Netns, "ping", "-c1", "-W2", "8.8.8.8")
	// First nameserver from host resolv (usually ClusterIP DNS).
	if raw, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" {
				run("ping_nameserver_"+fields[1], "ip", "netns", "exec", info.Netns, "ping", "-c1", "-W2", fields[1])
				break
			}
		}
	}

	path := filepath.Join(g.RestoreRoot, name+".net.diag.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		log.Printf("gvisor restore %s: write network diag: %v", name, err)
		return
	}
	log.Printf("gvisor restore %s: network diag at %s (%d bytes)", name, path, b.Len())
}

func runIPCommand(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
