package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RuntimeBackend string

const RuntimeBackendCRI RuntimeBackend = "cri"

type CRIRuntimeConfig struct {
	// RuntimeHandler is the CRI runtime handler name configured in the Worker's
	// containerd. It is an opaque string (for example "runsc" or "runc");
	// SandboxFleet does not interpret runtime-specific values.
	// +kubebuilder:validation:MinLength=1
	RuntimeHandler string `json:"runtimeHandler"`
}

// +kubebuilder:validation:XValidation:rule="self.backend != 'cri' || has(self.cri)",message="cri configuration is required for the cri backend"
type RuntimeConfig struct {
	// +kubebuilder:validation:Enum=cri
	Backend RuntimeBackend `json:"backend"`

	// +optional
	CRI *CRIRuntimeConfig `json:"cri,omitempty"`
}

// SandboxPoolSpec defines a homogeneous group of Workers.
type SandboxPoolSpec struct {
	// +kubebuilder:validation:Minimum=0
	Workers int32 `json:"workers"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="slotsPerWorker is immutable"
	SlotsPerWorker int32 `json:"slotsPerWorker"`

	// SlotResources is the resource budget for each Slot.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="slotResources is immutable"
	SlotResources corev1.ResourceRequirements `json:"slotResources,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="runtime is immutable"
	Runtime RuntimeConfig `json:"runtime"`
}

type SandboxPoolStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	CurrentWorkers     int32              `json:"currentWorkers,omitempty"`
	ReadyWorkers       int32              `json:"readyWorkers,omitempty"`
	UsedSlots          int32              `json:"usedSlots,omitempty"`
	AvailableSlots     int32              `json:"availableSlots,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
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
	Items           []SandboxPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
