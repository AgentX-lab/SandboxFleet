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
	id, unlock, err := m.runningRuntime(req.SlotID, req.Identity)
	if err != nil {
		return false, err
	}
	defer unlock()
	return m.runtime.FileExists(ctx, id, absPath)
}

func (m *SlotManager) ListSandboxFiles(ctx context.Context, req SandboxFileRequest) ([]SandboxFileEntry, error) {
	absPath, err := sandboxruntime.ResolveUnderRoot(DefaultFilesRoot, req.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	id, unlock, err := m.runningRuntime(req.SlotID, req.Identity)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := m.runtime.ListFiles(ctx, id, absPath)
	if err != nil {
		return nil, err
	}
	result := make([]SandboxFileEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, SandboxFileEntry{Name: entry.Name, Type: entry.Type})
	}
	return result, nil
}

func (m *SlotManager) ReadSandboxFile(ctx context.Context, req SandboxFileRequest) ([]byte, error) {
	absPath, err := sandboxruntime.ResolveUnderRoot(DefaultFilesRoot, req.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	id, unlock, err := m.runningRuntime(req.SlotID, req.Identity)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return m.runtime.ReadFile(ctx, id, absPath)
}

func (m *SlotManager) WriteSandboxFile(ctx context.Context, req SandboxFileRequest, content []byte) error {
	if err := sandboxruntime.ValidateWriteName(req.Path); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if len(content) > MaxFileBytes {
		return fmt.Errorf("%w: content exceeds %d bytes", ErrInvalidRequest, MaxFileBytes)
	}
	absPath := path.Join(DefaultFilesRoot, req.Path)
	id, unlock, err := m.runningRuntime(req.SlotID, req.Identity)
	if err != nil {
		return err
	}
	defer unlock()
	return m.runtime.WriteFile(ctx, id, absPath, content)
}

func (m *SlotManager) runningRuntime(slotID int32, identity SandboxIdentity) (sandboxruntime.ID, func(), error) {
	if err := validateIdentity(identity); err != nil {
		return sandboxruntime.ID{}, nil, err
	}
	current, err := m.getSlot(slotID)
	if err != nil {
		return sandboxruntime.ID{}, nil, err
	}

	current.lock.Lock()
	if current.state == slot.StateFree || current.sandbox.UID != identity.UID {
		current.lock.Unlock()
		return sandboxruntime.ID{}, nil, ErrSandboxNotFound
	}
	if current.state != slot.StateRunning || current.runtimeRef == nil {
		current.lock.Unlock()
		return sandboxruntime.ID{}, nil, fmt.Errorf("%w: sandbox is not running", ErrInvalidRequest)
	}
	id := *current.runtimeRef
	return id, current.lock.Unlock, nil
}
