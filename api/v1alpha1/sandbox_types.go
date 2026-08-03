package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const SandboxFinalizer = "sandboxfleet.io/runtime-cleanup"

type SandboxPhase string

const (
	SandboxPhasePending  SandboxPhase = "Pending"
	SandboxPhaseStarting SandboxPhase = "Starting"
	SandboxPhaseRunning  SandboxPhase = "Running"
	SandboxPhaseStopping SandboxPhase = "Stopping"
	SandboxPhaseFailed   SandboxPhase = "Failed"
)

const (
	ConditionReady        = "Ready"
	ConditionScheduled    = "Scheduled"
	ConditionWorkersReady = "WorkersReady"
)

type ContainerSpec struct {
	// +kubebuilder:validation:MinLength=1
	Image   string          `json:"image"`
	Command []string        `json:"command,omitempty"`
	Args    []string        `json:"args,omitempty"`
	Env     []corev1.EnvVar `json:"env,omitempty"`
}

// SandboxSpec defines one execution environment.
type SandboxSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="poolRef is immutable"
	PoolRef string `json:"poolRef"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="container is immutable"
	Container ContainerSpec `json:"container"`
}

// Assignment identifies the Worker and Slot assigned to a Sandbox.
type Assignment struct {
	Worker string `json:"worker"`
	SlotID int32  `json:"slotID"`
}

type SandboxStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              SandboxPhase       `json:"phase,omitempty"`
	Assignment         *Assignment        `json:"assignment,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sf
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=`.status.assignment.worker`
// +kubebuilder:printcolumn:name="Slot",type=integer,JSONPath=`.status.assignment.slotID`
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSpec   `json:"spec"`
	Status SandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
