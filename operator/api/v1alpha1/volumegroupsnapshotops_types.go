// volumegroupsnapshotops_types.go declares VolumeGroupSnapshotOps, the one-shot
// operation on a VolumeGroupSnapshot (design-consistency-groups.md §7.4). Its one
// action, Restore, creates one PersistentVolumeClaim per member snapshot of the
// target's generation and waits for every claim to bind.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeGroupSnapshotOpsAction is the operation to perform on the target.
// +kubebuilder:validation:Enum=Restore
type VolumeGroupSnapshotOpsAction string

const (
	// VolumeGroupSnapshotOpsActionRestore restores every member of the target's
	// generation into a new PersistentVolumeClaim.
	VolumeGroupSnapshotOpsActionRestore VolumeGroupSnapshotOpsAction = "Restore"
)

// VolumeGroupSnapshotOpsPhase is the lifecycle phase of the operation.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type VolumeGroupSnapshotOpsPhase string

const (
	VolumeGroupSnapshotOpsPhasePending   VolumeGroupSnapshotOpsPhase = "Pending"
	VolumeGroupSnapshotOpsPhaseRunning   VolumeGroupSnapshotOpsPhase = "Running"
	VolumeGroupSnapshotOpsPhaseSucceeded VolumeGroupSnapshotOpsPhase = "Succeeded"
	VolumeGroupSnapshotOpsPhaseFailed    VolumeGroupSnapshotOpsPhase = "Failed"
)

// VolumeGroupSnapshotOpsStep is one step of a running operation. The enum is the
// union of every action's steps. Which steps belong to which action is declared
// by the action's state-machine graph rather than by this type.
// +kubebuilder:validation:Enum=Validating;CreatingClaims;WaitingForBind
type VolumeGroupSnapshotOpsStep string

const (
	VolumeGroupSnapshotOpsStepValidating     VolumeGroupSnapshotOpsStep = "Validating"
	VolumeGroupSnapshotOpsStepCreatingClaims VolumeGroupSnapshotOpsStep = "CreatingClaims"
	VolumeGroupSnapshotOpsStepWaitingForBind VolumeGroupSnapshotOpsStep = "WaitingForBind"
)

// VolumeGroupSnapshotOpsStepSnapshot is the durable position of the action's
// state machine: the step and the deadline it expires at.
type VolumeGroupSnapshotOpsStepSnapshot struct {
	// +optional
	State VolumeGroupSnapshotOpsStep `json:"state,omitempty"`
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
}

// RestoreOpsSpec carries the parameters of the Restore action.
type RestoreOpsSpec struct {
	// NamePrefix prefixes every restored claim's name:
	// <namePrefix>-<source PVC name>, the source name read from the member
	// snapshot's spec.source.persistentVolumeClaimName. Defaults to the
	// operation's own name.
	// +k8s:immutable
	// +optional
	NamePrefix string `json:"namePrefix,omitempty"`

	// StorageClassName is the class every restored claim requests. When empty,
	// each claim inherits the class of its member snapshot's source claim, and
	// the restore fails for a member whose source claim no longer exists.
	// +k8s:immutable
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// ConsistencyGroup labels every restored claim with
	// storage.simplyblock.io/consistency-group: <value>, so the clones form a
	// new group at provisioning under the mandatory placement rule. Empty
	// leaves the clones as independent, mutually consistent volumes.
	// +k8s:immutable
	// +optional
	ConsistencyGroup string `json:"consistencyGroup,omitempty"`

	// EnablePartialRestore restores the members an incomplete generation still
	// has instead of failing the operation. Off by default: an incomplete
	// generation fails, naming the missing members.
	// +k8s:immutable
	// +optional
	EnablePartialRestore bool `json:"enablePartialRestore,omitempty"`
}

// VolumeGroupSnapshotOpsSpec defines the requested operation. The whole spec is
// immutable: the object is a request.
type VolumeGroupSnapshotOpsSpec struct {
	// VolumeGroupSnapshotRef names the VolumeGroupSnapshot, in this namespace,
	// the operation acts on. Resolved at admission: a create naming a
	// VolumeGroupSnapshot that does not exist is rejected.
	// +k8s:immutable
	// +kubebuilder:validation:Required
	VolumeGroupSnapshotRef string `json:"volumeGroupSnapshotRef"`

	// Action is the operation to perform.
	// +k8s:immutable
	// +kubebuilder:validation:Required
	Action VolumeGroupSnapshotOpsAction `json:"action"`

	// Restore carries the parameters of the Restore action. Ignored for any
	// other action.
	// +k8s:immutable
	// +optional
	Restore *RestoreOpsSpec `json:"restore,omitempty"`
}

// RestoredMemberStatus records one member snapshot and the claim restored
// from it.
type RestoredMemberStatus struct {
	// VolumeSnapshotName is the member VolumeSnapshot the claim restores from.
	VolumeSnapshotName string `json:"volumeSnapshotName"`

	// PersistentVolumeClaimName is the restored claim.
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName"`

	// Bound reports whether the restored claim has bound.
	// +optional
	Bound bool `json:"bound,omitempty"`
}

// VolumeGroupSnapshotOpsStatus holds the observed state of the operation.
type VolumeGroupSnapshotOpsStatus struct {
	// Phase is the high-level lifecycle phase.
	// +optional
	Phase VolumeGroupSnapshotOpsPhase `json:"phase,omitempty"`

	// Step is the durable position of the action's state machine.
	// +optional
	Step VolumeGroupSnapshotOpsStepSnapshot `json:"step,omitempty"`

	// Message is the reason the phase is what it is: one sentence, replaced as
	// the phase moves.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// MembersExpected is the member count of the target generation.
	// +optional
	MembersExpected int `json:"membersExpected,omitempty"`

	// MembersBound is how many restored claims have bound.
	// +optional
	MembersBound int `json:"membersBound,omitempty"`

	// Members records each member snapshot and its restored claim.
	// +optional
	Members []RestoredMemberStatus `json:"members,omitempty"`

	// StartedAt is when the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation finished, successfully or not.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=vgsops
// +kubebuilder:printcolumn:name="GroupSnapshot",type=string,JSONPath=".spec.volumeGroupSnapshotRef"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=".status.membersBound"
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=".status.message"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// VolumeGroupSnapshotOps is a one-shot operation on a VolumeGroupSnapshot,
// analogous to a Kubernetes Job: it drives its action to completion, records
// the result, and is then inert. The Restore action creates one
// PersistentVolumeClaim per member snapshot of the target's generation and
// waits for every claim to bind.
type VolumeGroupSnapshotOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeGroupSnapshotOpsSpec   `json:"spec,omitempty"`
	Status VolumeGroupSnapshotOpsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeGroupSnapshotOpsList contains a list of VolumeGroupSnapshotOps.
type VolumeGroupSnapshotOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeGroupSnapshotOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VolumeGroupSnapshotOps{}, &VolumeGroupSnapshotOpsList{})
}
