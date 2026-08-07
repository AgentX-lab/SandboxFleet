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

	// Match containerd CRI / gVisor specutils annotation keys.
	annotationContainerType = "io.kubernetes.cri.container-type"
	annotationContainerName = "io.kubernetes.cri.container-name"
	annotationSandboxID     = "io.kubernetes.cri.sandbox-id"
	containerTypeSandbox    = "sandbox"
	containerTypeContainer  = "container"
)

// GVisor implements memory snapshot/restore with runsc.
//
// Parent (CRI) stays under SourceRoot with --leave-running.
// Child restore uses a private RestoreRoot + netns on sf-br0 (same 10.88.0.0/16
// plan as Kata) so nested fork children do not share host ports.
//
// CRI sandboxes are multi-container (pause + app). Restore follows substrate:
// create+restore pause, then create+restore each app against the same checkpoint.
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
	// Sidecar listing for multi-container restore (uploaded with checkpoint files).
	if err := g.writeContainersForSave(req); err != nil {
		return fmt.Errorf("write containers metadata: %w", err)
	}
	return nil
}

// writeContainersForSave records pause+app (or N apps) next to the checkpoint.
func (g *GVisor) writeContainersForSave(req SaveRequest) error {
	if name, ok := StripPrefix(req.ID.Value, gvisorIDPrefix); ok {
		if containers, err := readGVisorContainersFile(filepath.Join(g.RestoreRoot, name)); err == nil {
			return writeGVisorContainersFile(req.DestDir, containers)
		}
	}
	appName := req.AppContainerName
	if appName == "" {
		return fmt.Errorf("AppContainerName is required to record gVisor restore containers")
	}
	return writeGVisorContainersFile(req.DestDir, gvisorCRIContainers(appName))
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
		// Checkpoint the sandbox root (pause); same as substrate checkpointing "pause".
		return filepath.Join(g.RestoreRoot, name), "pause"
	}
	return g.SourceRoot, runtimeID
}

func (g *GVisor) LoadSnapshot(ctx context.Context, req LoadRequest) (sandboxruntime.ID, error) {
	if req.SourceDir == "" {
		return sandboxruntime.ID{}, fmt.Errorf("source dir is required")
	}
	containers, err := readGVisorContainersFile(req.SourceDir)
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("read restore containers: %w", err)
	}
	name := RestoredName(req.Identity)
	root := filepath.Join(g.RestoreRoot, name)
	_ = os.RemoveAll(root)
	_ = os.Remove(g.restoreNetInfoPath(name))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return sandboxruntime.ID{}, err
	}

	cleanupFailed := func() {
		for i := len(containers) - 1; i >= 0; i-- {
			_ = g.run(ctx, []string{"--root", root, "delete", "-f", containers[i].ID})
		}
		if info, infoErr := g.loadRestoreNetInfo(name); infoErr == nil {
			_ = deleteRestoreNetwork(ctx, info)
		}
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
	}

	log.Printf("gvisor restore %s: setup network slot=%d containers=%d", name, req.SlotID, len(containers))
	netInfo, err := g.createRestoreNetwork(ctx, req.SlotID, name)
	if err != nil {
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("setup restore network: %w", err)
	}

	debugDir := filepath.Join(root, "debug")
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		cleanupFailed()
		return sandboxruntime.ID{}, fmt.Errorf("mkdir debug log dir: %w", err)
	}

	rootCID := ""
	for _, c := range containers {
		if c.Sandbox {
			rootCID = c.ID
			break
		}
	}
	if rootCID == "" {
		rootCID = containers[0].ID
		containers[0].Sandbox = true
	}

	// Substrate order: create+restore each container against the same image-path.
	// gVisor only finishes restore after the last container's Restore RPC (N/N).
	for i, c := range containers {
		bundleDir := filepath.Join(root, "bundles", c.ID)
		if err := writeGVisorRestoreBundle(bundleDir, c, rootCID); err != nil {
			cleanupFailed()
			return sandboxruntime.ID{}, fmt.Errorf("write restore bundle %s: %w", c.ID, err)
		}
		_ = ensureRestoreCgroup(c.ID)
		createArgs := gvisorCreateArgs(root, bundleDir, c.ID, debugDir)
		log.Printf("gvisor restore %s: create %s (%d/%d)", name, c.ID, i+1, len(containers))
		if err := g.runInNetworkNamespace(ctx, netInfo.Netns, createArgs, filepath.Join(root, "create-"+c.ID+".log")); err != nil {
			cleanupFailed()
			return sandboxruntime.ID{}, fmt.Errorf("runsc create %s: %w", c.ID, err)
		}
		restoreArgs := gvisorRestoreArgs(root, bundleDir, req.SourceDir, c.ID, debugDir)
		log.Printf("gvisor restore %s: restore %s image=%s", name, c.ID, req.SourceDir)
		if err := g.runInNetworkNamespace(ctx, netInfo.Netns, restoreArgs, filepath.Join(root, "restore-"+c.ID+".log")); err != nil {
			cleanupFailed()
			return sandboxruntime.ID{}, fmt.Errorf("runsc restore %s: %w", c.ID, err)
		}
	}

	if err := writeGVisorContainersFile(root, containers); err != nil {
		cleanupFailed()
		return sandboxruntime.ID{}, fmt.Errorf("persist restore containers: %w", err)
	}
	log.Printf("gvisor restore %s: done (%d containers)", name, len(containers))
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
// until DeleteRestored. Placeholder OCI bundles use --restore-spec-validation=ignore.
// App readiness is a caller/e2e concern (readyz), not wait --restore.
func gvisorRestoreArgs(root, bundleDir, imagePath, sandboxName, debugDir string) []string {
	args := []string{
		"--root", root,
		"--network=host",
		"--restore-spec-validation=ignore",
	}
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
// open config.json. Annotations match CRI so gVisor remaps containers by name.
// Memory/process state still comes from the checkpoint; rootfs is a placeholder.
func writeGVisorRestoreBundle(bundleDir string, c gvisorRestoreContainer, rootCID string) error {
	if err := os.MkdirAll(filepath.Join(bundleDir, "rootfs"), 0o755); err != nil {
		return err
	}
	annotations := map[string]string{}
	if c.Sandbox {
		annotations[annotationContainerType] = containerTypeSandbox
	} else {
		annotations[annotationContainerType] = containerTypeContainer
		annotations[annotationSandboxID] = rootCID
		if c.Name != "" {
			annotations[annotationContainerName] = c.Name
		}
	}
	cfg := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"user": map[string]any{"uid": 0, "gid": 0},
			"args": []string{"sleep", "3600"},
			"cwd":  "/",
		},
		"root":        map[string]any{"path": "rootfs"},
		"annotations": annotations,
		"linux": map[string]any{
			"cgroupsPath": restoreCgroupsPath(c.ID),
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
	containers, err := readGVisorContainersFile(root)
	if err != nil {
		// Best-effort: older restores only had a single container named `name`.
		containers = []gvisorRestoreContainer{{ID: name, Sandbox: true}}
	}
	var first error
	for i := len(containers) - 1; i >= 0; i-- {
		if err := g.run(ctx, []string{"--root", root, "delete", "-f", containers[i].ID}); err != nil && first == nil {
			first = err
		}
	}
	_ = os.RemoveAll(root)

	if info, infoErr := g.loadRestoreNetInfo(name); infoErr == nil {
		_ = deleteRestoreNetwork(ctx, info)
		_ = os.Remove(g.restoreNetInfoPath(name))
	}
	return first
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
	containers, err := readGVisorContainersFile(root)
	if err != nil {
		containers = []gvisorRestoreContainer{{ID: name}}
	}
	cid := gvisorAppContainerID(containers)
	if cid == "" {
		return sandboxruntime.ExecResult{}, fmt.Errorf("no app container for restored %q", name)
	}
	info, err := g.loadRestoreNetInfo(name)
	if err != nil {
		return sandboxruntime.ExecResult{}, fmt.Errorf("load restore netns: %w", err)
	}
	// create/restore ran inside this netns with --network=host; exec must too.
	debugDir := filepath.Join(root, "debug")
	args := append([]string{"--root", root, "--network=host"}, gvisorDebugFlags(debugDir)...)
	args = append(args, append([]string{"exec", cid}, req.Command...)...)
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
