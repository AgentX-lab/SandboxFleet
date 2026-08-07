//go:build linux

package snapshotter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

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

// createRestoreNetwork creates sf-br0 + netns + veth with a unique 10.88.0.x address.
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
func (g *GVisor) runInNetworkNamespace(ctx context.Context, nsName string, args []string, logPath string) error {
	return doInNamedNetNS(nsName, func() error {
		cmd := exec.CommandContext(ctx, g.RunscPath, args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if logPath != "" {
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
				cmd.Stdout = f
				cmd.Stderr = io.MultiWriter(f, &stderr)
				defer f.Close()
			}
		}
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s %s: %w: %s", g.RunscPath, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return nil
	})
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
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
	// Idempotent: add MASQUERADE only if missing.
	check := exec.CommandContext(ctx, "iptables", "-t", "nat", "-C", "POSTROUTING",
		"-s", guestSubnet, "!", "-o", guestBridge, "-j", "MASQUERADE")
	if check.Run() == nil {
		return
	}
	_ = exec.CommandContext(ctx, "iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", guestSubnet, "!", "-o", guestBridge, "-j", "MASQUERADE").Run()
}

func runIPCommand(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
