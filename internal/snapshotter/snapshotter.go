package snapshotter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

// Snapshotter is the Worker-local memory checkpoint/restore adapter.
//
// Plain language:
//   - SaveSnapshot = take a memory photo of a running sandbox onto disk
//   - LoadSnapshot = recreate a running sandbox from that photo
//   - ExecRestored  = run a command inside a restored sandbox (CRI cannot see it)
type Snapshotter interface {
	FormatVersion() string
	SaveSnapshot(ctx context.Context, req SaveRequest) error
	LoadSnapshot(ctx context.Context, req LoadRequest) (sandboxruntime.ID, error)
	DeleteRestored(ctx context.Context, id sandboxruntime.ID) error
	ExecRestored(ctx context.Context, id sandboxruntime.ID, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error)
}

type SaveRequest struct {
	ID          sandboxruntime.ID
	DestDir     string
	ContainerID string // CRI container id: Kata meta + gVisor app rootfs pack
	// AppContainerName is the CRI io.kubernetes.cri.container-name of the
	// workload container (Sandbox metadata.name). Used by gVisor to write
	// sandboxfleet-containers.json so restore can recreate pause+app.
	AppContainerName string
}

type LoadRequest struct {
	SourceDir string
	Identity  sandboxruntime.SandboxIdentity
	SlotID    int32
}

// Registry picks the Snapshotter for a Worker's runtimeHandler.
type Registry struct {
	byHandler map[string]Snapshotter
}

func NewRegistry(entries map[string]Snapshotter) *Registry {
	out := make(map[string]Snapshotter, len(entries))
	for k, v := range entries {
		out[k] = v
	}
	return &Registry{byHandler: out}
}

func (r *Registry) For(handler string) (Snapshotter, error) {
	if r == nil {
		return nil, fmt.Errorf("snapshotter registry is nil")
	}
	s, ok := r.byHandler[handler]
	if !ok || s == nil {
		return nil, fmt.Errorf("no snapshotter for runtimeHandler %q", handler)
	}
	return s, nil
}

// RestoredName builds a short unique name for a restored instance.
func RestoredName(identity sandboxruntime.SandboxIdentity) string {
	base := identity.Name
	if base == "" {
		base = "restored"
	}
	uid := string(identity.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return filepath.Base(base) + "-" + uid
}

func StripPrefix(value, prefix string) (string, bool) {
	if strings.HasPrefix(value, prefix) {
		return strings.TrimPrefix(value, prefix), true
	}
	return "", false
}

func IsRestoredID(id sandboxruntime.ID) bool {
	return strings.HasPrefix(id.Value, "runsc:") || strings.HasPrefix(id.Value, "kata:")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstExistingPath returns the first non-empty path that exists as a file.
// Bare names (no slash) are returned as-is for PATH lookup by exec.
func firstExistingPath(values ...string) string {
	var fallback string
	for _, v := range values {
		if v == "" {
			continue
		}
		fallback = v
		if !strings.ContainsRune(v, os.PathSeparator) {
			return v
		}
		if st, err := os.Stat(v); err == nil && !st.IsDir() {
			return v
		}
	}
	return fallback
}
