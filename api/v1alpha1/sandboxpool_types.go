package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RuntimeBackend string

const RuntimeBackendCRI RuntimeBackend = "cri"

// SnapshotterKind selects the Worker memory checkpoint/restore adapter.
// It is independent of RuntimeHandler: never infer one from the other.
type SnapshotterKind string

const (
	SnapshotterGVisor SnapshotterKind = "gvisor"
	SnapshotterKata   SnapshotterKind = "kata"
)

type CRIRuntimeConfig struct {
	// RuntimeHandler is the CRI runtime handler name configured in the Worker's
	// containerd. It is an opaque string (for example "runsc", "runc", or "kata");
	// SandboxFleet does not interpret runtime-specific values.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	RuntimeHandler string `json:"runtimeHandler"`

	// Snapshotter selects how this Pool's Workers create and restore memory snapshots.
	// Explicit on purpose: do not derive this from runtimeHandler.
	// +kubebuilder:validation:Enum=gvisor;kata
	Snapshotter SnapshotterKind `json:"snapshotter"`

	// HostDevices lists host paths to mount into every Worker Pod for this Pool
	// (for example "/dev/kvm"). Runtime-agnostic: the controller mounts whatever
	// is declared here and does not special-case handler names.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	HostDevices []string `json:"hostDevices,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.backend != 'cri' || has(self.cri)",message="cri configuration is required for the cri backend"
type RuntimeConfig struct {
	// +kubebuilder:validation:Enum=cri
	Backend RuntimeBackend `json:"backend"`

	// +optional
	CRI *CRIRuntimeConfig `json:"cri,omitempty"`
}

// SlotProfile is a named, fixed resource specification for Slots.
type SlotProfile struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Resources is the per-Slot budget. Immutable after the Profile is created.
	// Resource changes are rejected by Worker ApplySlots for existing Slot IDs.
	Resources corev1.ResourceRequirements `json:"resources"`
}

// SlotGroup declares how many Slots of one Profile each Worker of a Template has.
type SlotGroup struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Profile string `json:"profile"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=256
	Count int32 `json:"count"`
}

// WorkerTemplate defines a homogeneous set of Worker Pods and their Slot layout.
type WorkerTemplate struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	Replicas int32 `json:"replicas"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Slots []SlotGroup `json:"slots"`
}

// Runtime and profile/template names are immutable. Profile resources are enforced
// at apply time (existing Slot IDs cannot change resources).
// +kubebuilder:validation:XValidation:rule="self.runtime == oldSelf.runtime",message="runtime is immutable"
// +kubebuilder:validation:XValidation:rule="size(self.slotProfiles) == size(oldSelf.slotProfiles) && oldSelf.slotProfiles.all(o, self.slotProfiles.exists(p, p.name == o.name))",message="slotProfile names are immutable"
// +kubebuilder:validation:XValidation:rule="size(self.workerTemplates) == size(oldSelf.workerTemplates) && oldSelf.workerTemplates.all(o, self.workerTemplates.exists(t, t.name == o.name))",message="workerTemplate names are immutable"
type SandboxPoolSpec struct {
	Runtime RuntimeConfig `json:"runtime"`

	// SnapshotStorage is where fork snapshots for this Pool are uploaded.
	// +optional
	SnapshotStorage *SnapshotStorageSpec `json:"snapshotStorage,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	SlotProfiles []SlotProfile `json:"slotProfiles"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	WorkerTemplates []WorkerTemplate `json:"workerTemplates"`
}

// SnapshotStorageSpec configures object storage for SandboxSnapshot bytes.
type SnapshotStorageSpec struct {
	// +optional
	S3 *S3SnapshotStorage `json:"s3,omitempty"`
}

type S3SnapshotStorage struct {
	// Endpoint is optional (empty = AWS). MinIO/e2e set http://minio:9000.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`
	// +optional
	Region string `json:"region,omitempty"`
	// CredentialsSecretRef points at a Secret with keys accessKeyID and secretAccessKey.
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// WorkerTemplateStatus reports observed replica counts for one Template.
type WorkerTemplateStatus struct {
	Name          string `json:"name"`
	Replicas      int32  `json:"replicas"`
	ReadyReplicas int32  `json:"readyReplicas"`

	// AppliedSlots is the Slot layout currently applied for this Template's Workers.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	AppliedSlots []AppliedSlot `json:"appliedSlots,omitempty"`
}

// AppliedSlot is one Slot in a Template's applied topology.
type AppliedSlot struct {
	ID int32 `json:"id"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Profile string `json:"profile"`
}

// SlotProfileStatus reports aggregate Slot capacity for one Profile.
type SlotProfileStatus struct {
	Name      string `json:"name"`
	Total     int32  `json:"total"`
	Used      int32  `json:"used"`
	Available int32  `json:"available"`
}

type SandboxPoolStatus struct {
	ObservedGeneration int64                  `json:"observedGeneration,omitempty"`
	CurrentWorkers     int32                  `json:"currentWorkers,omitempty"`
	ReadyWorkers       int32                  `json:"readyWorkers,omitempty"`
	UsedSlots          int32                  `json:"usedSlots,omitempty"`
	AvailableSlots     int32                  `json:"availableSlots,omitempty"`
	Templates          []WorkerTemplateStatus `json:"templates,omitempty"`
	Profiles           []SlotProfileStatus    `json:"profiles,omitempty"`
	Conditions         []metav1.Condition     `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sfp
// +kubebuilder:printcolumn:name="Workers",type=integer,JSONPath=`.status.currentWorkers`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyWorkers`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableSlots`
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxPoolSpec   `json:"spec"`
	Status SandboxPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SandboxPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
