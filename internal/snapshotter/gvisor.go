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
	// Sidecar listing + per-container rootfs tars (uploaded with checkpoint files).
	if err := g.writeContainersForSave(req); err != nil {
		return fmt.Errorf("write containers metadata: %w", err)
	}
	return nil
}

// writeContainersForSave packs pause+app rootfs and records restore metadata.
func (g *GVisor) writeContainersForSave(req SaveRequest) error {
	containers, err := g.containersForSave(req)
	if err != nil {
		return err
	}
	for i := range containers {
		src, err := g.resolveRootfsForSave(req, containers[i])
		if err != nil {
			return fmt.Errorf("resolve rootfs %s: %w", containers[i].ID, err)
		}
		tarName := gvisorRootfsTarName(containers[i].ID)
		if err := packRootfsTar(src, filepath.Join(req.DestDir, tarName)); err != nil {
			return fmt.Errorf("pack rootfs %s from %s: %w", containers[i].ID, src, err)
		}
		containers[i].RootfsTar = tarName
	}
	return writeGVisorContainersFile(req.DestDir, containers)
}

func (g *GVisor) containersForSave(req SaveRequest) ([]gvisorRestoreContainer, error) {
	if name, ok := StripPrefix(req.ID.Value, gvisorIDPrefix); ok {
		if containers, err := readGVisorContainersFile(filepath.Join(g.RestoreRoot, name)); err == nil {
			for i := range containers {
				containers[i].RootfsTar = ""
			}
			return containers, nil
		}
	}
	appName := req.AppContainerName
	if appName == "" {
		return nil, fmt.Errorf("AppContainerName is required to record gVisor restore containers")
	}
	return gvisorCRIContainers(appName), nil
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

	// On failure: keep create/restore/*.log + debug/ under <name>-failed for e2e
	// runsc-state-*.tar (cleanup used to RemoveAll the root and wipe evidence).
	preserveFailedLogs := func(stage string) string {
		keep := filepath.Join(g.RestoreRoot, name+"-failed")
		_ = os.RemoveAll(keep)
		if err := os.MkdirAll(keep, 0o755); err != nil {
			log.Printf("gvisor restore %s: mkdir failed-logs: %v", name, err)
			return ""
		}
		_ = os.WriteFile(filepath.Join(keep, "failed-stage.txt"), []byte(stage+"\n"), 0o600)
		debugDir := filepath.Join(root, "debug")
		if _, err := os.Stat(debugDir); err == nil {
			_ = os.Rename(debugDir, filepath.Join(keep, "debug"))
		}
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
				continue
			}
			_ = os.Rename(filepath.Join(root, e.Name()), filepath.Join(keep, e.Name()))
		}
		log.Printf("gvisor restore %s: preserved failure logs at %s (stage=%s)", name, keep, stage)
		return keep
	}

	cleanupFailed := func(stage string) string {
		keep := preserveFailedLogs(stage)
		for i := len(containers) - 1; i >= 0; i-- {
			_ = g.run(ctx, []string{"--root", root, "delete", "-f", containers[i].ID})
		}
		if info, infoErr := g.loadRestoreNetInfo(name); infoErr == nil {
			_ = deleteRestoreNetwork(ctx, info)
		}
		_ = os.Remove(g.restoreNetInfoPath(name))
		_ = os.RemoveAll(root)
		return keep
	}

	log.Printf("gvisor restore %s: setup network slot=%d containers=%d", name, req.SlotID, len(containers))
	netInfo, err := g.createRestoreNetwork(ctx, req.SlotID, name)
	if err != nil {
		_ = os.RemoveAll(root)
		return sandboxruntime.ID{}, fmt.Errorf("setup restore network: %w", err)
	}

	debugDir := filepath.Join(root, "debug")
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		_ = cleanupFailed("mkdir-debug")
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
		if err := writeGVisorRestoreBundle(bundleDir, c, rootCID, req.SourceDir); err != nil {
			keep := cleanupFailed("write-bundle-" + c.ID)
			return sandboxruntime.ID{}, fmt.Errorf("write restore bundle %s: %w (logs: %s)", c.ID, err, keep)
		}
		_ = ensureRestoreCgroup(c.ID)
		createLog := filepath.Join(root, "create-"+c.ID+".log")
		createArgs := gvisorCreateArgs(root, bundleDir, c.ID, debugDir)
		log.Printf("gvisor restore %s: create %s (%d/%d)", name, c.ID, i+1, len(containers))
		if err := g.runInNetworkNamespace(ctx, netInfo.Netns, createArgs, createLog); err != nil {
			keep := cleanupFailed("create-" + c.ID)
			return sandboxruntime.ID{}, fmt.Errorf("runsc create %s: %w (logs: %s)", c.ID, err, keep)
		}
		restoreLog := filepath.Join(root, "restore-"+c.ID+".log")
		restoreArgs := gvisorRestoreArgs(root, bundleDir, req.SourceDir, c.ID, debugDir)
		log.Printf("gvisor restore %s: restore %s image=%s", name, c.ID, req.SourceDir)
		if err := g.runInNetworkNamespace(ctx, netInfo.Netns, restoreArgs, restoreLog); err != nil {
			// Best-effort: dump restore log to worker stdout before preserve/cleanup.
			if raw, rerr := os.ReadFile(restoreLog); rerr == nil && len(raw) > 0 {
				log.Printf("gvisor restore %s: restore-%s.log (%d bytes):\n%s", name, c.ID, len(raw), truncateForLog(string(raw), 8<<10))
			} else {
				log.Printf("gvisor restore %s: restore-%s.log empty or unreadable: %v", name, c.ID, rerr)
			}
			keep := cleanupFailed("restore-" + c.ID)
			return sandboxruntime.ID{}, fmt.Errorf("runsc restore %s: %w (logs: %s)", c.ID, err, keep)
		}
	}

	if err := writeGVisorContainersFile(root, containers); err != nil {
		keep := cleanupFailed("persist-containers")
		return sandboxruntime.ID{}, fmt.Errorf("persist restore containers: %w (logs: %s)", err, keep)
	}
	log.Printf("gvisor restore %s: done (%d containers)", name, len(containers))
	return sandboxruntime.ID{Value: gvisorIDPrefix + name}, nil
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
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
// --restore-spec-validation=ignore must be set at create/boot so the sandbox
// process inherits it; restore-only flags do not update the running sentry.
func gvisorCreateArgs(root, bundleDir, sandboxName, debugDir string) []string {
	args := []string{
		"--root", root,
		"--network=host",
		"--restore-spec-validation=ignore",
	}
	args = append(args, gvisorDebugFlags(debugDir)...)
	return append(args, "create", "--bundle", bundleDir, sandboxName)
}

// gvisorRestoreArgs builds argv for `runsc restore` after create.
// Matches substrate: --background --direct --detach. Checkpoint dir must stay
// until DeleteRestored. Placeholder OCI bundles use --restore-spec-validation=ignore.
// App readiness is a caller/e2e concern (readyz).
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

// writeGVisorRestoreBundle writes an OCI bundle with a real rootfs unpacked from
// the snapshot tar (Kata-style), plus CRI annotations and /etc bind mounts.
// Memory/process state still comes from the checkpoint.
func writeGVisorRestoreBundle(bundleDir string, c gvisorRestoreContainer, rootCID, checkpointDir string) error {
	rootfsDir := filepath.Join(bundleDir, "rootfs")
	if c.RootfsTar == "" {
		return fmt.Errorf("container %s missing rootfsTar in %s", c.ID, gvisorContainersFile)
	}
	if err := unpackRootfsTar(filepath.Join(checkpointDir, c.RootfsTar), rootfsDir); err != nil {
		return fmt.Errorf("unpack %s: %w", c.RootfsTar, err)
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
		// Match containerd/CRI checkpoint ociVersion (seen as 1.1.0 in CI).
		"ociVersion": "1.1.0",
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
	if !c.Sandbox {
		mounts, err := gvisorRestoreEtcMounts(bundleDir, c.Name)
		if err != nil {
			return err
		}
		cfg["mounts"] = mounts
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, "config.json"), raw, 0o600)
}

// gvisorRestoreEtcMounts creates host-side etc files and returns OCI bind mounts.
// Destination paths must match CRI UniqueIDs saved in the checkpoint.
func gvisorRestoreEtcMounts(bundleDir, hostname string) ([]map[string]any, error) {
	if hostname == "" {
		hostname = "sandbox"
	}
	etcDir := filepath.Join(bundleDir, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		return nil, err
	}
	hostsPath := filepath.Join(etcDir, "hosts")
	hostnamePath := filepath.Join(etcDir, "hostname")
	resolvPath := filepath.Join(etcDir, "resolv.conf")
	if err := os.WriteFile(hostsPath, []byte("127.0.0.1\tlocalhost\n::1\tlocalhost\n"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(hostnamePath, []byte(hostname+"\n"), 0o644); err != nil {
		return nil, err
	}
	resolv := []byte("nameserver 8.8.8.8\n")
	if raw, err := os.ReadFile("/etc/resolv.conf"); err == nil && len(raw) > 0 {
		resolv = raw
	}
	if err := os.WriteFile(resolvPath, resolv, 0o644); err != nil {
		return nil, err
	}
	bind := func(src, dst string) map[string]any {
		return map[string]any{
			"destination": dst,
			"type":        "bind",
			"source":      src,
			"options":     []string{"rbind", "rprivate", "ro"},
		}
	}
	return []map[string]any{
		bind(hostsPath, "/etc/hosts"),
		bind(hostnamePath, "/etc/hostname"),
		bind(resolvPath, "/etc/resolv.conf"),
	}, nil
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
