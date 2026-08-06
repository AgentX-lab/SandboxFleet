package snapshotter

import (
	"bytes"
	"context"
	"fmt"
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

	netInfo, err := g.createRestoreNetwork(ctx, req.SlotID, name)
	if err != nil {
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("setup restore network: %w", err)
	}

	// Match substrate / gVisor docs: create (registers OCI bundle) then restore
	// (loads checkpoint into that container). restore alone has no config.json.
	bundleDir := filepath.Join(root, "bundle")
	if err := writeGVisorRestoreBundle(bundleDir); err != nil {
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("write restore bundle: %w", err)
	}
	createArgs := gvisorCreateArgs(root, bundleDir, name)
	if err := g.runInNetworkNamespace(ctx, netInfo.Netns, createArgs); err != nil {
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("runsc create: %w", err)
	}
	restoreArgs := gvisorRestoreArgs(root, bundleDir, req.SourceDir, name)
	if err := g.runInNetworkNamespace(ctx, netInfo.Netns, restoreArgs); err != nil {
		_ = g.run(ctx, []string{"--root", root, "delete", "-f", name})
		_ = deleteRestoreNetwork(ctx, netInfo)
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("runsc restore: %w", err)
	}
	return sandboxruntime.ID{Value: gvisorIDPrefix + name}, nil
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
func gvisorRestoreArgs(root, bundleDir, imagePath, sandboxName string) []string {
	return []string{
		"--root", root,
		"--network=host",
		"restore",
		"--bundle", bundleDir,
		"--image-path", imagePath,
		"--detach",
		sandboxName,
	}
}

// writeGVisorRestoreBundle writes a minimal OCI bundle so runsc FetchSpec can
// open config.json. Memory/process state still comes from the checkpoint;
// rootfs is an empty placeholder (same idea as substrate composing a bundle
// before create+restore, without imagecache overlays).
func writeGVisorRestoreBundle(bundleDir string) error {
	if err := os.MkdirAll(filepath.Join(bundleDir, "rootfs"), 0o755); err != nil {
		return err
	}
	// Minimal OCI runtime-spec; --network=host so no network namespace here.
	const config = `{
  "ociVersion": "1.0.2",
  "process": {
    "user": {"uid": 0, "gid": 0},
    "args": ["sleep", "3600"],
    "cwd": "/"
  },
  "root": {"path": "rootfs"},
  "linux": {
    "namespaces": [
      {"type": "pid"},
      {"type": "ipc"},
      {"type": "uts"},
      {"type": "mount"}
    ]
  }
}
`
	return os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(config), 0o600)
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
