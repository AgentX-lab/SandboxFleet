package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/snapshotstore"
	"github.com/AgentNaut/SandboxFleet/internal/snapshotter"
)

// ObjectStorageConfig is the Worker-side view of Pool snapshotStorage credentials.
type ObjectStorageConfig struct {
	Endpoint        string `json:"endpoint,omitempty"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region,omitempty"`
	AccessKeyID     string `json:"accessKeyID"`
	SecretAccessKey string `json:"secretAccessKey"`
	UsePathStyle    bool   `json:"usePathStyle,omitempty"`
}

type CreateSnapshotRequest struct {
	SlotID      int32                         `json:"slotID"`
	Identity    SandboxIdentity               `json:"identity"`
	StoragePath string                        `json:"storagePath"`
	Storage     ObjectStorageConfig           `json:"storage"`
	Runtime     string                        `json:"runtime"`
	Pool        string                        `json:"pool,omitempty"`
	Container   sandboxv1alpha1.ContainerSpec `json:"container,omitempty"`
}

type CreateSnapshotResult struct {
	StoragePath   string   `json:"storagePath"`
	SnapshotFiles []string `json:"snapshotFiles"`
	Digest        string   `json:"digest,omitempty"`
	SizeBytes     int64    `json:"sizeBytes"`
	FormatVersion string   `json:"formatVersion"`
	Runtime       string   `json:"runtime"`
}

type RestoreFromSnapshotRequest struct {
	SlotID      int32               `json:"slotID"`
	Identity    SandboxIdentity     `json:"identity"`
	StoragePath string              `json:"storagePath"`
	Storage     ObjectStorageConfig `json:"storage"`
	Runtime     string              `json:"runtime"`
}

type DeleteSnapshotObjectsRequest struct {
	StoragePath   string              `json:"storagePath"`
	SnapshotFiles []string            `json:"snapshotFiles,omitempty"`
	Storage       ObjectStorageConfig `json:"storage"`
}

// CreateSnapshot: pause parent → SaveSnapshot → upload manifest+*.zstd → parent keeps running.
func (m *SlotManager) CreateSnapshot(ctx context.Context, req CreateSnapshotRequest) (CreateSnapshotResult, error) {
	if err := validateIdentity(req.Identity); err != nil {
		return CreateSnapshotResult{}, err
	}
	if req.StoragePath == "" || req.Storage.Bucket == "" {
		return CreateSnapshotResult{}, fmt.Errorf("%w: storagePath and storage.bucket are required", ErrInvalidRequest)
	}
	handler := req.Runtime
	if handler == "" {
		handler = m.runtimeHandler()
	}
	snap, err := m.snapshotters.For(handler)
	if err != nil {
		return CreateSnapshotResult{}, err
	}

	current, err := m.getSlot(req.SlotID)
	if err != nil {
		return CreateSnapshotResult{}, err
	}
	current.lock.Lock()
	defer current.lock.Unlock()
	if current.state != slot.StateRunning || current.runtimeRef == nil || current.sandbox.UID != req.Identity.UID {
		return CreateSnapshotResult{}, fmt.Errorf("%w: sandbox is not running on slot %d", ErrInvalidRequest, req.SlotID)
	}

	workDir, err := os.MkdirTemp("", "sandboxfleet-snapshot-*")
	if err != nil {
		return CreateSnapshotResult{}, err
	}
	defer os.RemoveAll(workDir)

	// CRI only knows cold-started sandboxes. Restored (nested-fork) parents keep
	// container id inside snapshotter state; SaveSnapshot fills it when needed.
	containerID := ""
	if !snapshotter.IsRestoredID(*current.runtimeRef) {
		containerID, err = primaryContainerID(ctx, m.runtime, *current.runtimeRef)
		if err != nil {
			return CreateSnapshotResult{}, fmt.Errorf("resolve container id: %w", err)
		}
	}
	if err := snap.SaveSnapshot(ctx, snapshotter.SaveRequest{
		ID:          *current.runtimeRef,
		DestDir:     workDir,
		ContainerID: containerID,
	}); err != nil {
		return CreateSnapshotResult{}, fmt.Errorf("save snapshot: %w", err)
	}

	store, err := openSnapshotStorage(req.Storage)
	if err != nil {
		return CreateSnapshotResult{}, err
	}
	manifest := snapshotstore.Manifest{
		RuntimeHandler: handler,
		FormatVersion:  snap.FormatVersion(),
		SourceSandbox: snapshotstore.SourceSandbox{
			Namespace: req.Identity.Namespace,
			Name:      req.Identity.Name,
			UID:       string(req.Identity.UID),
		},
		PoolRef:   req.Pool,
		Container: req.Container,
		CreatedAt: time.Now().UTC(),
	}
	size, err := store.Upload(ctx, req.StoragePath, workDir, manifest)
	if err != nil {
		_ = store.Delete(ctx, req.StoragePath, nil)
		return CreateSnapshotResult{}, fmt.Errorf("upload snapshot: %w", err)
	}
	uploaded, err := store.GetManifest(ctx, req.StoragePath)
	if err != nil {
		return CreateSnapshotResult{}, err
	}
	return CreateSnapshotResult{
		StoragePath:   req.StoragePath,
		SnapshotFiles: uploaded.SnapshotFiles,
		SizeBytes:     size,
		FormatVersion: snap.FormatVersion(),
		Runtime:       handler,
	}, nil
}

// RestoreFromSnapshot downloads prefix objects and LoadSnapshot into a reserved slot.
func (m *SlotManager) RestoreFromSnapshot(ctx context.Context, req RestoreFromSnapshotRequest) error {
	if err := validateIdentity(req.Identity); err != nil {
		return err
	}
	if req.StoragePath == "" || req.Storage.Bucket == "" {
		return fmt.Errorf("%w: storagePath and storage.bucket are required", ErrInvalidRequest)
	}
	handler := req.Runtime
	if handler == "" {
		handler = m.runtimeHandler()
	}
	if local := m.runtimeHandler(); local != "" && handler != local {
		return fmt.Errorf("%w: snapshot runtime %q does not match worker %q", ErrInvalidRequest, handler, local)
	}
	snap, err := m.snapshotters.For(handler)
	if err != nil {
		return err
	}

	current, err := m.getSlot(req.SlotID)
	if err != nil {
		return err
	}
	current.lock.Lock()
	defer current.lock.Unlock()
	if current.sandbox.UID != req.Identity.UID {
		return fmt.Errorf("%w: slot %d is not reserved by sandbox %q", ErrSlotConflict, req.SlotID, req.Identity.UID)
	}
	if current.state == slot.StateRunning && current.runtimeRef != nil {
		return nil
	}
	if current.state != slot.StateReserved && current.state != slot.StateStarting {
		return fmt.Errorf("slot %d cannot restore from state %q", req.SlotID, current.state)
	}

	workDir, err := os.MkdirTemp("", "sandboxfleet-restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	store, err := openSnapshotStorage(req.Storage)
	if err != nil {
		return err
	}
	checkpointDir := filepath.Join(workDir, "checkpoint")
	manifest, err := store.Download(ctx, req.StoragePath, checkpointDir)
	if err != nil {
		return fmt.Errorf("download snapshot: %w", err)
	}
	if manifest.RuntimeHandler != "" && manifest.RuntimeHandler != handler {
		return fmt.Errorf("%w: archive handler %q does not match %q", ErrInvalidRequest, manifest.RuntimeHandler, handler)
	}

	current.state = slot.StateStarting
	runtimeID, err := snap.LoadSnapshot(ctx, snapshotter.LoadRequest{
		SourceDir: checkpointDir,
		Identity:  sandboxruntime.SandboxIdentity{Namespace: req.Identity.Namespace, Name: req.Identity.Name, UID: req.Identity.UID},
		SlotID:    req.SlotID,
	})
	if err != nil {
		current.state = slot.StateReserved
		return fmt.Errorf("load snapshot: %w", err)
	}
	current.runtimeRef = &runtimeID
	current.restored = true
	current.state = slot.StateRunning
	return nil
}

func (m *SlotManager) DeleteSnapshotObjects(ctx context.Context, req DeleteSnapshotObjectsRequest) error {
	if req.StoragePath == "" || req.Storage.Bucket == "" {
		return fmt.Errorf("%w: storagePath and storage.bucket are required", ErrInvalidRequest)
	}
	store, err := openSnapshotStorage(req.Storage)
	if err != nil {
		return err
	}
	return store.Delete(ctx, req.StoragePath, req.SnapshotFiles)
}

func (m *SlotManager) runtimeHandler() string {
	if m.config.Runtime.CRI != nil {
		return m.config.Runtime.CRI.RuntimeHandler
	}
	return ""
}

// primaryContainerIDResolver is optional; only CRI runtimes implement it (Kata meta).
type primaryContainerIDResolver interface {
	PrimaryContainerID(ctx context.Context, id sandboxruntime.ID) (string, error)
}

func primaryContainerID(ctx context.Context, rt sandboxruntime.Runtime, id sandboxruntime.ID) (string, error) {
	resolver, ok := rt.(primaryContainerIDResolver)
	if !ok {
		return "", nil
	}
	return resolver.PrimaryContainerID(ctx, id)
}

func openSnapshotStorage(cfg ObjectStorageConfig) (*snapshotstore.SnapshotStorage, error) {
	objects, err := snapshotstore.NewS3(snapshotstore.S3Config{
		Endpoint:        cfg.Endpoint,
		Bucket:          cfg.Bucket,
		Region:          cfg.Region,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		UsePathStyle:    cfg.UsePathStyle || cfg.Endpoint != "",
	})
	if err != nil {
		return nil, err
	}
	return snapshotstore.New(objects), nil
}
