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
	createArgs := gvisorCreateArgs(root, bundleDir, name)
	log.Printf("gvisor restore %s: runsc create", name)
	if err := g.runInNetworkNamespace(ctx, netInfo.Netns, createArgs, filepath.Join(root, "create.log")); err != nil {
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("runsc create: %w", err)
	}
	restoreArgs := gvisorRestoreArgs(root, bundleDir, req.SourceDir, name)
	log.Printf("gvisor restore %s: runsc restore image=%s", name, req.SourceDir)
	if err := g.runInNetworkNamespace(ctx, netInfo.Netns, restoreArgs, filepath.Join(root, "restore.log")); err != nil {
		_ = g.run(ctx, []string{"--root", root, "delete", "-f", name})
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("runsc restore: %w", err)
	}
	// --detach can return before status is running; wait like substrate readyz
	// so callers (ExecRestored) do not race "container not started".
	log.Printf("gvisor restore %s: wait running", name)
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := g.waitRunning(waitCtx, root, name); err != nil {
		_ = g.run(ctx, []string{"--root", root, "delete", "-f", name})
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("wait running after restore: %w", err)
	}
	log.Printf("gvisor restore %s: done", name)
	return sandboxruntime.ID{Value: gvisorIDPrefix + name}, nil
}

// waitRunning polls `runsc state` until status is running or ctx ends.
func (g *GVisor) waitRunning(ctx context.Context, root, sandboxName string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		status, err := g.containerStatus(ctx, root, sandboxName)
		if err == nil && strings.EqualFold(status, "running") {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = status
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("last status %q: %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (g *GVisor) containerStatus(ctx context.Context, root, sandboxName string) (string, error) {
	cmd := exec.CommandContext(ctx, g.RunscPath, "--root", root, "state", sandboxName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseRunscStateStatus(stdout.Bytes())
}

func parseRunscStateStatus(raw []byte) (string, error) {
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("parse state: %w", err)
	}
	if st.Status == "" {
		return "", fmt.Errorf("state missing status: %s", strings.TrimSpace(string(raw)))
	}
	return st.Status, nil
}

// gvisorCreateArgs builds argv for `runsc create` before restore.
func gvisorCreateArgs(root, bundleDir, sandboxName string) []string {
	return []string{
		"--root", root,
		"--network=host",
		"create",
		"--bundle", bundleDir,
		sandboxName,
	}
}

// gvisorRestoreArgs builds argv for `runsc restore` after create.
// --direct for the fast filestore path; --detach for CLI return. LoadSnapshot
// waits for running before returning (substrate-equivalent of readyz).
func gvisorRestoreArgs(root, bundleDir, imagePath, sandboxName string) []string {
	return []string{
		"--root", root,
		"--network=host",
		"restore",
		"--bundle", bundleDir,
		"--image-path", imagePath,
		"--direct",
		"--detach",
		sandboxName,
	}
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
	args := append([]string{"--root", root, "exec", name}, req.Command...)
	cmd := exec.CommandContext(execCtx, g.RunscPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := sandboxruntime.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = int32(exitErr.ExitCode())
			return result, nil
		}
		return result, fmt.Errorf("runsc exec: %w", err)
	}
	return result, nil
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
