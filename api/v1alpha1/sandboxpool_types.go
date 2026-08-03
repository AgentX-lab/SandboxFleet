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
	// +kubebuilder:validation:MaxLength=253
	RuntimeHandler string `json:"runtimeHandler"`
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

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	SlotProfiles []SlotProfile `json:"slotProfiles"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	WorkerTemplates []WorkerTemplate `json:"workerTemplates"`
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
