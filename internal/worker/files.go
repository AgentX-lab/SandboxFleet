package worker

import (
	"context"
	"fmt"
	"path"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
)

// Re-export filesystem policy constants for HTTP/API layers.
const (
	DefaultFilesRoot  = sandboxruntime.DefaultFilesRoot
	MaxFileBytes      = sandboxruntime.MaxFileBytes
	FileTypeFile      = sandboxruntime.FileTypeFile
	FileTypeDirectory = sandboxruntime.FileTypeDirectory
)

type SandboxFileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type SandboxFileRequest struct {
	SlotID   int32           `json:"slotID"`
	Identity SandboxIdentity `json:"identity"`
	Path     string          `json:"path"`
}

func (m *SlotManager) ExistsSandboxFile(ctx context.Context, req SandboxFileRequest) (bool, error) {
	absPath, err := sandboxruntime.ResolveUnderRoot(DefaultFilesRoot, req.Path)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	current, unlock, err := m.runningSlot(req.SlotID, req.Identity)
	if err != nil {
		return false, err
	}
	defer unlock()
	return m.sandboxFSFor(current).Exists(ctx, absPath)
}

func (m *SlotManager) ListSandboxFiles(ctx context.Context, req SandboxFileRequest) ([]SandboxFileEntry, error) {
	absPath, err := sandboxruntime.ResolveUnderRoot(DefaultFilesRoot, req.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	current, unlock, err := m.runningSlot(req.SlotID, req.Identity)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return m.sandboxFSFor(current).List(ctx, absPath)
}

func (m *SlotManager) ReadSandboxFile(ctx context.Context, req SandboxFileRequest) ([]byte, error) {
	absPath, err := sandboxruntime.ResolveUnderRoot(DefaultFilesRoot, req.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	current, unlock, err := m.runningSlot(req.SlotID, req.Identity)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return m.sandboxFSFor(current).Read(ctx, absPath)
}

func (m *SlotManager) WriteSandboxFile(ctx context.Context, req SandboxFileRequest, content []byte) error {
	if err := sandboxruntime.ValidateWriteName(req.Path); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if len(content) > MaxFileBytes {
		return fmt.Errorf("%w: content exceeds %d bytes", ErrInvalidRequest, MaxFileBytes)
	}
	absPath := path.Join(DefaultFilesRoot, req.Path)
	current, unlock, err := m.runningSlot(req.SlotID, req.Identity)
	if err != nil {
		return err
	}
	defer unlock()
	return m.sandboxFSFor(current).Write(ctx, absPath, content)
}

// sandboxFSFor picks the FS backend for a locked running slot.
func (m *SlotManager) sandboxFSFor(current *managedSlot) sandboxFS {
	if current.restored {
		id := *current.runtimeRef
		return &execSandboxFS{
			exec: func(ctx context.Context, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
				return m.execRestored(ctx, id, req)
			},
		}
	}
	return &criSandboxFS{rt: m.runtime, id: *current.runtimeRef}
}

func (m *SlotManager) runningSlot(slotID int32, identity SandboxIdentity) (*managedSlot, func(), error) {
	if err := validateIdentity(identity); err != nil {
		return nil, nil, err
	}
	current, err := m.getSlot(slotID)
	if err != nil {
		return nil, nil, err
	}
	current.lock.Lock()
	if current.state == slot.StateFree || current.sandbox.UID != identity.UID {
		current.lock.Unlock()
		return nil, nil, ErrSandboxNotFound
	}
	if current.state != slot.StateRunning || current.runtimeRef == nil {
		current.lock.Unlock()
		return nil, nil, fmt.Errorf("%w: sandbox is not running", ErrInvalidRequest)
	}
	return current, current.lock.Unlock, nil
}

func (m *SlotManager) execRestored(ctx context.Context, id sandboxruntime.ID, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	snap, err := m.snapshotters.For(m.runtimeHandler())
	if err != nil {
		return sandboxruntime.ExecResult{}, err
	}
	return snap.ExecRestored(ctx, id, req)
}
