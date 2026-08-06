package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const SandboxSnapshotFinalizer = "sandboxfleet.io/snapshot-cleanup"

type SandboxSnapshotPhase string

const (
	SandboxSnapshotPhasePending SandboxSnapshotPhase = "Pending"
	SandboxSnapshotPhaseReady   SandboxSnapshotPhase = "Ready"
	SandboxSnapshotPhaseFailed  SandboxSnapshotPhase = "Failed"
)

// SandboxSnapshotSpec identifies who created this immutable snapshot.
type SandboxSnapshotSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sourceSandbox is immutable"
	SourceSandbox string `json:"sourceSandbox"`

	// Pool is the SandboxPool name that owns snapshot storage for this snapshot.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pool is immutable"
	Pool string `json:"pool"`
}

// SandboxSnapshotStatus records where the snapshot bytes live.
type SandboxSnapshotStatus struct {
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	Phase              SandboxSnapshotPhase `json:"phase,omitempty"`
	// StoragePath is the S3/MinIO path holding manifest.json + *.zstd files.
	StoragePath string `json:"storagePath,omitempty"`
	// SnapshotFiles lists uncompressed member names (same as manifest.snapshotFiles).
	SnapshotFiles []string `json:"snapshotFiles,omitempty"`
	// Runtime is the CRI runtime used to create this snapshot (for example "runsc" or "kata").
	Runtime       string             `json:"runtime,omitempty"`
	FormatVersion string             `json:"formatVersion,omitempty"`
	SizeBytes     int64              `json:"sizeBytes,omitempty"`
	SourceWorker  string             `json:"sourceWorker,omitempty"`
	Message       string             `json:"message,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sfs
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.status.runtime`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.storagePath`
type SandboxSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSnapshotSpec   `json:"spec"`
	Status SandboxSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SandboxSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxSnapshot{}, &SandboxSnapshotList{})
}
