package worker

import "github.com/AgentNaut/SandboxFleet/internal/workerapi"

// Aliases keep Worker call sites stable while wire types live in workerapi
// (so Controller/httpapi clients do not import this package's snapshotter deps).

var (
	ErrInvalidRequest    = workerapi.ErrInvalidRequest
	ErrSlotNotFound      = workerapi.ErrSlotNotFound
	ErrSandboxNotFound   = workerapi.ErrSandboxNotFound
	ErrSlotConflict      = workerapi.ErrSlotConflict
	ErrSlotConfigInvalid = workerapi.ErrSlotConfigInvalid
)

const (
	DefaultFilesRoot  = workerapi.DefaultFilesRoot
	MaxFileBytes      = workerapi.MaxFileBytes
	FileTypeFile      = workerapi.FileTypeFile
	FileTypeDirectory = workerapi.FileTypeDirectory
)

type (
	SandboxIdentity              = workerapi.SandboxIdentity
	SandboxSlotRef               = workerapi.SandboxSlotRef
	StartSandboxRequest          = workerapi.StartSandboxRequest
	SandboxInfo                  = workerapi.SandboxInfo
	ExecSandboxRequest           = workerapi.ExecSandboxRequest
	ExecSandboxResult            = workerapi.ExecSandboxResult
	SandboxFileEntry             = workerapi.SandboxFileEntry
	SandboxFileRequest           = workerapi.SandboxFileRequest
	ObjectStorageConfig          = workerapi.ObjectStorageConfig
	CreateSnapshotRequest        = workerapi.CreateSnapshotRequest
	CreateSnapshotResult         = workerapi.CreateSnapshotResult
	RestoreFromSnapshotRequest   = workerapi.RestoreFromSnapshotRequest
	DeleteSnapshotObjectsRequest = workerapi.DeleteSnapshotObjectsRequest
)
