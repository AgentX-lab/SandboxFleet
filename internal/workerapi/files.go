package workerapi

import sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"

// Filesystem policy constants (shared by Worker HTTP and SDK).
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
