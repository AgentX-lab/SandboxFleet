package workerapi

import sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"

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
