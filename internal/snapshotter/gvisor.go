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

	// Match substrate atelet resolveEnv default PATH so runsc exec can find
	// binaries (e.g. python for e2e readyz) when PATH would otherwise be empty.
	gvisorDefaultPATH = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// Public DNS for restored children on sf-br0 (cluster DNS is unreachable).
	gvisorRestoreResolvConf = "nameserver 8.8.8.8\n"
)

// GVisor implements memory snapshot/restore with runsc.
//
// Parent (CRI) stays under SourceRoot with --leave-running.
// Child restore uses a private RestoreRoot + netns on sf-br0 (10.89.0.0/16,
// distinct from CNI 10.88.0.0/16) so nested fork children do not share host ports.
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

// writeContainersForSave records pause+app image refs for substrate-style restore.
func (g *GVisor) writeContainersForSave(req SaveRequest) error {
	containers, err := g.containersForSave(req)
	if err != nil {
		return err
	}
	if err := fillGVisorContainerImages(containers, req.AppImage); err != nil {
		return err
	}
	return writeGVisorContainersFile(req.DestDir, containers)
}

func (g *GVisor) containersForSave(req SaveRequest) ([]gvisorRestoreContainer, error) {
	if name, ok := StripPrefix(req.ID.Value, gvisorIDPrefix); ok {
		if containers, err := readGVisorContainersFile(filepath.Join(g.RestoreRoot, name)); err == nil {
			return containers, nil
		}
	}
	appName := req.AppContainerName
	if appName == "" {
		return nil, fmt.Errorf("AppContainerName is required to record gVisor restore containers")
	}
	return gvisorCRIContainers(appName, req.AppImage), nil
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
	if err := fillGVisorContainerImages(containers, req.AppImage); err != nil {
		return sandboxruntime.ID{}, err
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
			teardownImageRootfs(filepath.Join(root, "bundles", containers[i].ID))
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
		if err := writeGVisorRestoreBundle(ctx, bundleDir, c, rootCID); err != nil {
			keep := cleanupFailed("write-bundle-" + c.ID)
			return sandboxruntime.ID{}, fmt.Errorf("write restore bundle %s: %w (logs: %s)", c.ID, err, keep)
		}
		_ = ensureRestoreCgroup(c.ID)
		createLog := filepath.Join(root, "create-"+c.ID+".log")
		createArgs := gvisorCreateArgs(root, bundleDir, c.ID, debugDir, c.Sandbox)
		log.Printf("gvisor restore %s: create %s (%d/%d)", name, c.ID, i+1, len(containers))
		if err := g.runInNetworkNamespace(ctx, netInfo.Netns, createArgs, createLog); err != nil {
			keep := cleanupFailed("create-" + c.ID)
			return sandboxruntime.ID{}, fmt.Errorf("runsc create %s: %w (logs: %s)", c.ID, err, keep)
		}
		restoreLog := filepath.Join(root, "restore-"+c.ID+".log")
		restoreArgs := gvisorRestoreArgs(root, bundleDir, req.SourceDir, c.ID, debugDir, c.Sandbox)
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
//
// sandbox=true (pause): --overlay2=none so restore does not register an extra
// private MF. CRI checkpoints only list app MFs (savedMFOwners=[name:/]); the
// default root:self on pause yields __no_name_0:/ or pause:/ and fails
// consistency checks. Host image overlay already supplies the rootfs
// (substrate SetupBundleRootfs).
func gvisorCreateArgs(root, bundleDir, sandboxName, debugDir string, sandbox bool) []string {
	args := []string{
		"--root", root,
		"--network=host",
		"--restore-spec-validation=ignore",
	}
	if sandbox {
		args = append(args, "--overlay2=none")
	}
	args = append(args, gvisorDebugFlags(debugDir)...)
	return append(args, "create", "--bundle", bundleDir, sandboxName)
}

// gvisorRestoreArgs builds argv for `runsc restore` after create.
// Matches substrate: --background --direct --detach. Checkpoint dir must stay
// until DeleteRestored. Placeholder OCI bundles use --restore-spec-validation=ignore.
// App readiness is a caller/e2e concern (readyz).
// sandbox=true applies the same --overlay2=none as create (see gvisorCreateArgs).
func gvisorRestoreArgs(root, bundleDir, imagePath, sandboxName, debugDir string, sandbox bool) []string {
	args := []string{
		"--root", root,
		"--network=host",
		"--restore-spec-validation=ignore",
	}
	if sandbox {
		args = append(args, "--overlay2=none")
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

// writeGVisorRestoreBundle mounts a substrate-style image overlay rootfs and
// writes OCI config with CRI annotations + /etc bind mounts.
func writeGVisorRestoreBundle(ctx context.Context, bundleDir string, c gvisorRestoreContainer, rootCID string) error {
	if c.Image == "" {
		return fmt.Errorf("container %s missing image in %s", c.ID, gvisorContainersFile)
	}
	if err := setupImageRootfs(ctx, c.Image, bundleDir); err != nil {
		return fmt.Errorf("setup image rootfs %q: %w", c.Image, err)
	}
	if err := writeGVisorRestoreConfig(bundleDir, c, rootCID); err != nil {
		teardownImageRootfs(bundleDir)
		return err
	}
	return nil
}

// writeGVisorRestoreConfig writes config.json for an already-materialized rootfs.
func writeGVisorRestoreConfig(bundleDir string, c gvisorRestoreContainer, rootCID string) error {
	// CRI restore: sandbox is container-type=sandbox with no container-name
	// (__no_name_0). Apps get container-type=container + sandbox-id + name.
	annotations := map[string]string{}
	if c.Sandbox {
		annotations[annotationContainerType] = containerTypeSandbox
	} else {
		annotations[annotationContainerType] = containerTypeContainer
		annotations[annotationSandboxID] = rootCID
	}
	if c.Name != "" {
		annotations[annotationContainerName] = c.Name
	}
	cfg := map[string]any{
		// Match containerd/CRI checkpoint ociVersion (seen as 1.1.0 in CI).
		"ociVersion": "1.1.0",
		"process": map[string]any{
			"user": map[string]any{"uid": 0, "gid": 0},
			"args": []string{"sleep", "3600"},
			"cwd":  "/",
			"env":  []string{gvisorDefaultPATH},
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
	hostname := c.Name
	if c.Sandbox {
		hostname = "pause"
	}
	// Match substrate buildActorOCISpec: every container (pause + app) gets
	// /etc/resolv.conf bind. CRI checkpoints also need hosts/hostname and the
	// files present under the rootfs gofer for CompleteRestore walks.
	mounts, err := gvisorRestoreEtcMounts(bundleDir, hostname)
	if err != nil {
		return err
	}
	cfg["mounts"] = mounts
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, "config.json"), raw, 0o600)
}

// gvisorRestoreEtcMounts adds CRI hosts/hostname binds and a resolv.conf bind.
// Substrate binds host /etc/resolv.conf because actors share a netns that can
// reach cluster DNS. Restored children use sf-br0 (10.89.0.0/16) where that
// ClusterIP is unreachable, so we bind a bundle-local resolv with 8.8.8.8
// (egress via existing MASQUERADE). Files are also written under rootfs/etc
// for gofer walks of "__no_name_0:/etc/resolv.conf".
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
	if err := os.WriteFile(resolvPath, []byte(gvisorRestoreResolvConf), 0o644); err != nil {
		return nil, err
	}
	if err := ensureRootfsEtcFiles(bundleDir, hostname); err != nil {
		return nil, err
	}
	return []map[string]any{
		{
			"destination": "/etc/hosts",
			"type":        "bind",
			"source":      hostsPath,
			"options":     []string{"rbind", "rprivate", "ro"},
		},
		{
			"destination": "/etc/hostname",
			"type":        "bind",
			"source":      hostnamePath,
			"options":     []string{"rbind", "rprivate", "ro"},
		},
		{
			"destination": "/etc/resolv.conf",
			"type":        "bind",
			"source":      resolvPath,
			"options":     []string{"ro"},
		},
	}, nil
}

// ensureRootfsEtcFiles writes network files into the overlay upper. CRI creates
// these under the sandbox rootfs; checkpoint restore walks them via the root
// gofer even when OCI also bind-mounts the same paths.
func ensureRootfsEtcFiles(bundleDir, hostname string) error {
	rootEtc := filepath.Join(bundleDir, "rootfs", "etc")
	if err := os.MkdirAll(rootEtc, 0o755); err != nil {
		return err
	}
	if hostname == "" {
		hostname = "sandbox"
	}
	files := map[string][]byte{
		"hosts":       []byte("127.0.0.1\tlocalhost\n::1\tlocalhost\n"),
		"hostname":    []byte(hostname + "\n"),
		"resolv.conf": []byte(gvisorRestoreResolvConf),
	}
	for name, body := range files {
		path := filepath.Join(rootEtc, name)
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			continue
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}
	return nil
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
		teardownImageRootfs(filepath.Join(root, "bundles", containers[i].ID))
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
