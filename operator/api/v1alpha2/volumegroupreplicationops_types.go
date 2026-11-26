// volumegroupreplicationops_types.go declares VolumeGroupReplicationOps, the one-shot
// operation on an application's VolumeGroupReplication. Its one action, Fence,
// fences the consistency group on this site after the group was failed over to
// another site.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeGroupReplicationOpsAction is the operation to perform on the target.
// +kubebuilder:validation:Enum=Fence
type VolumeGroupReplicationOpsAction string

const (
	// VolumeGroupReplicationOpsActionFence fences every member of the group on
	// this site and stops its replication, with no final snapshot.
	VolumeGroupReplicationOpsActionFence VolumeGroupReplicationOpsAction = "Fence"
)

// VolumeGroupReplicationOpsPhase is the lifecycle phase of the operation.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type VolumeGroupReplicationOpsPhase string

const (
	VolumeGroupReplicationOpsPhasePending   VolumeGroupReplicationOpsPhase = "Pending"
	VolumeGroupReplicationOpsPhaseRunning   VolumeGroupReplicationOpsPhase = "Running"
	VolumeGroupReplicationOpsPhaseSucceeded VolumeGroupReplicationOpsPhase = "Succeeded"
	VolumeGroupReplicationOpsPhaseFailed    VolumeGroupReplicationOpsPhase = "Failed"
)

// VolumeGroupReplicationOpsStep is one step of a running operation.
// +kubebuilder:validation:Enum=Validating;Fencing;Reporting;Completed
type VolumeGroupReplicationOpsStep string

const (
	VolumeGroupReplicationOpsStepValidating VolumeGroupReplicationOpsStep = "Validating"
	VolumeGroupReplicationOpsStepFencing    VolumeGroupReplicationOpsStep = "Fencing"
	VolumeGroupReplicationOpsStepReporting  VolumeGroupReplicationOpsStep = "Reporting"
	// VolumeGroupReplicationOpsStepCompleted is the terminal step of an operation that succeeded.
	VolumeGroupReplicationOpsStepCompleted VolumeGroupReplicationOpsStep = "Completed"
)

// VolumeGroupReplicationOpsStepSnapshot is the step the operation is in and the
// deadline it expires at.
type VolumeGroupReplicationOpsStepSnapshot struct {
	// +optional
	State VolumeGroupReplicationOpsStep `json:"state,omitempty"`
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
}

// VolumeGroupReplicationOpsSpec defines the requested operation. The whole spec
// is immutable: the object is a request.
type VolumeGroupReplicationOpsSpec struct {
	// VolumeGroupReplicationRef names the VolumeGroupReplication, in this
	// namespace, whose consistency group the operation acts on. A name that does
	// not resolve when the operation runs fails it.
	// +k8s:immutable
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	VolumeGroupReplicationRef string `json:"volumeGroupReplicationRef"`

	// Action is the operation to perform. Fence fences every member of the group
	// on this site and stops its replication, without taking a final snapshot.
	// Use it on a site whose application was failed over to another site while
	// this one was unreachable.
	// +k8s:immutable
	// +kubebuilder:validation:Required
	Action VolumeGroupReplicationOpsAction `json:"action"`
}

// FencedMemberStatus is one member volume of the fenced group.
type FencedMemberStatus struct {
	// VolumeID is the member's volume ID on this site's control plane.
	VolumeID string `json:"volumeID"`

	// Fenced reports that the member is fenced and no longer replicates.
	// +optional
	Fenced bool `json:"fenced,omitempty"`
}

// VolumeGroupReplicationOpsStatus holds the observed state of the operation.
type VolumeGroupReplicationOpsStatus struct {
	// Phase is the high-level lifecycle phase.
	// +optional
	Phase VolumeGroupReplicationOpsPhase `json:"phase,omitempty"`

	// Step is the step the operation is in, and the deadline it expires at.
	// +optional
	Step VolumeGroupReplicationOpsStepSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// GroupHandle is the consistency group the operation acts on,
	// cg:<cluster>:<group>, read from the VolumeGroupReplication when the
	// operation started. Later steps act on this value, so deleting the
	// VolumeGroupReplication does not change it.
	// +optional
	GroupHandle string `json:"groupHandle,omitempty"`

	// Members lists the group's member volumes after the fence.
	// +optional
	Members []FencedMemberStatus `json:"members,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation finished, successfully or not.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=vgrops
// +kubebuilder:printcolumn:name="GroupReplication",type=string,JSONPath=".spec.volumeGroupReplicationRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// VolumeGroupReplicationOps is a one-shot operation on the consistency group of a
// VolumeGroupReplication on this site. It drives its action to completion, records
// the result, and is then inert. The Fence action fences every member of the group
// and stops its replication, for a site whose application was failed over to
// another site.
type VolumeGroupReplicationOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeGroupReplicationOpsSpec   `json:"spec,omitempty"`
	Status VolumeGroupReplicationOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeGroupReplicationOpsList contains a list of VolumeGroupReplicationOps.
type VolumeGroupReplicationOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeGroupReplicationOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VolumeGroupReplicationOps{}, &VolumeGroupReplicationOpsList{})
}
