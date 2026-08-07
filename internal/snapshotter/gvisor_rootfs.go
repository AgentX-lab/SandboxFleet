package snapshotter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// gvisorRootfsTarName is the snapshot-local archive for one restore container's rootfs.
func gvisorRootfsTarName(containerID string) string {
	return "rootfs-" + containerID + ".tar"
}

func containerdTaskDir(containerID string) string {
	state := firstNonEmpty(os.Getenv("SANDBOXFLEET_CONTAINERD_STATE"), "/run/containerd")
	ns := firstNonEmpty(os.Getenv("SANDBOXFLEET_CONTAINERD_NAMESPACE"), "k8s.io")
	return filepath.Join(state, "io.containerd.runtime.v2.task", ns, containerID)
}

// resolveContainerdTaskRootfs finds the host rootfs for a CRI container/sandbox id.
func resolveContainerdTaskRootfs(containerID string) (string, error) {
	if containerID == "" {
		return "", fmt.Errorf("container id is empty")
	}
	base := containerdTaskDir(containerID)
	rootfs := filepath.Join(base, "rootfs")
	if dirExists(rootfs) {
		return rootfs, nil
	}
	cfgPath := filepath.Join(base, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", fmt.Errorf("containerd task rootfs for %q: %w", containerID, err)
	}
	var cfg struct {
		Root struct {
			Path string `json:"path"`
		} `json:"root"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", cfgPath, err)
	}
	if cfg.Root.Path == "" {
		return "", fmt.Errorf("empty root.path in %s", cfgPath)
	}
	p := cfg.Root.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	if !dirExists(p) {
		return "", fmt.Errorf("root.path %q for %q does not exist", p, containerID)
	}
	return p, nil
}

func (g *GVisor) resolveRootfsForSave(req SaveRequest, c gvisorRestoreContainer) (string, error) {
	if name, ok := StripPrefix(req.ID.Value, gvisorIDPrefix); ok {
		p := filepath.Join(g.RestoreRoot, name, "bundles", c.ID, "rootfs")
		if dirExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("restored rootfs missing at %s", p)
	}
	cid := req.ContainerID
	if c.Sandbox {
		cid = req.ID.Value
	}
	if cid == "" {
		return "", fmt.Errorf("ContainerID is required to pack app rootfs")
	}
	return resolveContainerdTaskRootfs(cid)
}
