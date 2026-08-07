package snapshotter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

const (
	gvisorFormatVersion = "runsc-checkpoint-v1"
	gvisorIDPrefix      = "runsc:"
)

// GVisor implements memory snapshot/restore with runsc.
//
// Parent (CRI) stays under SourceRoot with --leave-running.
// Child restore uses a private RestoreRoot + netns on sf-br0 (same 10.88.0.0/16
// plan as Kata) so nested fork children do not share host ports.
type GVisor struct {
	RunscPath   string
	SourceRoot  string
	RestoreRoot string
}

func NewGVisor() *GVisor {
	return &GVisor{
		RunscPath:   firstNonEmpty(os.Getenv("SANDBOXFLEET_RUNSC_PATH"), "runsc"),
		SourceRoot:  firstNonEmpty(os.Getenv("SANDBOXFLEET_RUNSC_ROOT"), "/run/containerd/runsc/k8s.io"),
		RestoreRoot: firstNonEmpty(os.Getenv("SANDBOXFLEET_RUNSC_RESTORE_ROOT"), "/var/lib/sandboxfleet/runsc"),
	}
}

func (g *GVisor) FormatVersion() string { return gvisorFormatVersion }

func (g *GVisor) SaveSnapshot(ctx context.Context, req SaveRequest) error {
	if req.ID.Value == "" {
		return fmt.Errorf("runtime id is required")
	}
	if err := os.MkdirAll(req.DestDir, 0o755); err != nil {
		return err
	}

	root, sandboxName := g.resolveCheckpointPaths(req.ID.Value)
	// runsc wants: checkpoint [flags] <container-id> (id must be last).
	// --network=host matches Worker runsc.toml so --leave-running can reattach.
	args := gvisorCheckpointArgs(root, req.DestDir, sandboxName)
	if err := g.run(ctx, args); err != nil {
		return fmt.Errorf("runsc checkpoint: %w", err)
	}
	return nil
}

// gvisorCheckpointArgs builds argv for `runsc checkpoint`.
// Container id is last; see https://gvisor.dev/docs/user_guide/checkpoint_restore/
func gvisorCheckpointArgs(root, imagePath, sandboxName string) []string {
	return []string{
		"--root", root,
		"--network=host",
		"checkpoint",
		"--image-path", imagePath,
		"--leave-running",
		sandboxName,
	}
}

// resolveCheckpointPaths picks runsc --root and sandbox name for CRI vs restored ids.
func (g *GVisor) resolveCheckpointPaths(runtimeID string) (root, sandboxName string) {
	if name, ok := StripPrefix(runtimeID, gvisorIDPrefix); ok {
		return filepath.Join(g.RestoreRoot, name), name
	}
	return g.SourceRoot, runtimeID
}

func (g *GVisor) LoadSnapshot(ctx context.Context, req LoadRequest) (sandboxruntime.ID, error) {
	if req.SourceDir == "" {
		return sandboxruntime.ID{}, fmt.Errorf("source dir is required")
	}
	name := RestoredName(req.Identity)
	root := filepath.Join(g.RestoreRoot, name)
	_ = os.RemoveAll(root)
	_ = os.Remove(g.restoreNetInfoPath(name))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return sandboxruntime.ID{}, err
	}

	log.Printf("gvisor restore %s: setup network slot=%d", name, req.SlotID)
	netInfo, err := g.createRestoreNetwork(ctx, req.SlotID, name)
	if err != nil {
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("setup restore network: %w", err)
	}

	// Match substrate / gVisor docs: create (registers OCI bundle) then restore
	// (loads checkpoint into that container). restore alone has no config.json.
	bundleDir := filepath.Join(root, "bundle")
	if err := writeGVisorRestoreBundle(bundleDir, name); err != nil {
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("write restore bundle: %w", err)
	}
	_ = ensureRestoreCgroup(name)
	debugDir := filepath.Join(root, "debug")
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("mkdir debug log dir: %w", err)
	}
	createArgs := gvisorCreateArgs(root, bundleDir, name, debugDir)
	log.Printf("gvisor restore %s: runsc create (debug=%s)", name, debugDir)
	if err := g.runInNetworkNamespace(ctx, netInfo.Netns, createArgs, filepath.Join(root, "create.log")); err != nil {
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("runsc create: %w", err)
	}
	restoreArgs := gvisorRestoreArgs(root, bundleDir, req.SourceDir, name, debugDir)
	log.Printf("gvisor restore %s: runsc restore image=%s", name, req.SourceDir)
	if err := g.runInNetworkNamespace(ctx, netInfo.Netns, restoreArgs, filepath.Join(root, "restore.log")); err != nil {
		_ = g.run(ctx, []string{"--root", root, "delete", "-f", name})
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("runsc restore: %w", err)
	}
	// Bound wait so a stuck `wait --restore` fails with debug logs instead of
	// hanging the Worker HTTP call until the controller client times out.
	waitCtx, waitCancel := context.WithTimeout(ctx, 90*time.Second)
	defer waitCancel()
	log.Printf("gvisor restore %s: wait --restore (timeout=90s, debug=%s)", name, debugDir)
	if err := g.runInNetworkNamespace(waitCtx, netInfo.Netns, gvisorWaitRestoreArgs(root, name, debugDir), filepath.Join(root, "wait-restore.log")); err != nil {
		// Preserve debug logs for e2e runsc-state-*.tar before tearing down.
		keepDebug := filepath.Join(g.RestoreRoot, name+"-debug-failed")
		_ = os.RemoveAll(keepDebug)
		if rerr := os.Rename(debugDir, keepDebug); rerr != nil {
			log.Printf("gvisor restore %s: preserve debug failed: %v", name, rerr)
		} else {
			log.Printf("gvisor restore %s: preserved debug logs at %s", name, keepDebug)
		}
		_ = g.run(ctx, []string{"--root", root, "delete", "-f", name})
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("runsc wait --restore: %w (debug logs: %s)", err, keepDebug)
	}
	log.Printf("gvisor restore %s: done", name)
	return sandboxruntime.ID{Value: gvisorIDPrefix + name}, nil
}

// gvisorDebugFlags enables runsc --debug into debugDir (must end with /).
// Collected via e2e runsc-state-*.tar under /var/lib/sandboxfleet/runsc.
func gvisorDebugFlags(debugDir string) []string {
	if debugDir == "" {
		return nil
	}
	if !strings.HasSuffix(debugDir, string(os.PathSeparator)) {
		debugDir += string(os.PathSeparator)
	}
	return []string{
		"--debug",
		"--debug-log", debugDir,
		"--alsologtostderr",
	}
}

// gvisorCreateArgs builds argv for `runsc create` before restore.
func gvisorCreateArgs(root, bundleDir, sandboxName, debugDir string) []string {
	args := []string{"--root", root, "--network=host"}
	args = append(args, gvisorDebugFlags(debugDir)...)
	return append(args, "create", "--bundle", bundleDir, sandboxName)
}

// gvisorRestoreArgs builds argv for `runsc restore` after create.
// Matches substrate: --background --direct --detach. Checkpoint dir must stay
// until DeleteRestored; LoadSnapshot then runs `wait --restore`. App readyz is
// still a caller/e2e concern.
func gvisorRestoreArgs(root, bundleDir, imagePath, sandboxName, debugDir string) []string {
	args := []string{"--root", root, "--network=host"}
	args = append(args, gvisorDebugFlags(debugDir)...)
	return append(args,
		"restore",
		"--bundle", bundleDir,
		"--image-path", imagePath,
		"--background",
		"--direct",
		"--detach",
		sandboxName,
	)
}

func gvisorWaitRestoreArgs(root, sandboxName, debugDir string) []string {
	args := []string{"--root", root, "--network=host"}
	args = append(args, gvisorDebugFlags(debugDir)...)
	return append(args, "wait", "--restore", sandboxName)
}

// writeGVisorRestoreBundle writes a minimal OCI bundle so runsc FetchSpec can
// open config.json. cgroupsPath mirrors substrate (colon-free → cgroupfs/v2).
// Memory/process state still comes from the checkpoint; rootfs is a placeholder.
func writeGVisorRestoreBundle(bundleDir, sandboxName string) error {
	if err := os.MkdirAll(filepath.Join(bundleDir, "rootfs"), 0o755); err != nil {
		return err
	}
	cfg := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"user": map[string]any{"uid": 0, "gid": 0},
			"args": []string{"sleep", "3600"},
			"cwd":  "/",
		},
		"root": map[string]any{"path": "rootfs"},
		"linux": map[string]any{
			"cgroupsPath": restoreCgroupsPath(sandboxName),
			"namespaces": []map[string]string{
				{"type": "pid"},
				{"type": "ipc"},
				{"type": "uts"},
				{"type": "mount"},
			},
		},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, "config.json"), raw, 0o600)
}

func (g *GVisor) DeleteRestored(ctx context.Context, id sandboxruntime.ID) error {
	name, ok := StripPrefix(id.Value, gvisorIDPrefix)
	if !ok {
		return fmt.Errorf("not a restored runsc id: %q", id.Value)
	}
	root := filepath.Join(g.RestoreRoot, name)
	err := g.run(ctx, []string{"--root", root, "delete", "-f", name})
	_ = os.RemoveAll(root)

	if info, infoErr := g.loadRestoreNetInfo(name); infoErr == nil {
		_ = deleteRestoreNetwork(ctx, info)
		_ = os.Remove(g.restoreNetInfoPath(name))
	}
	return err
}

func (g *GVisor) ExecRestored(ctx context.Context, id sandboxruntime.ID, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	name, ok := StripPrefix(id.Value, gvisorIDPrefix)
	if !ok {
		return sandboxruntime.ExecResult{}, fmt.Errorf("not a restored runsc id: %q", id.Value)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	root := filepath.Join(g.RestoreRoot, name)
	info, err := g.loadRestoreNetInfo(name)
	if err != nil {
		return sandboxruntime.ExecResult{}, fmt.Errorf("load restore netns: %w", err)
	}
	// create/restore ran inside this netns with --network=host; exec must too.
	debugDir := filepath.Join(root, "debug")
	args := append([]string{"--root", root, "--network=host"}, gvisorDebugFlags(debugDir)...)
	args = append(args, append([]string{"exec", name}, req.Command...)...)
	return g.execInNetworkNamespace(execCtx, info.Netns, args)
}

func (g *GVisor) run(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, g.RunscPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
