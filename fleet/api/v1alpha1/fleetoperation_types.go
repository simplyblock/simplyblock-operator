// FleetOperation: one operation from the storage group, shipped into one member,
// and what it does there.
//
// One kind carries every operation rather than one hub kind per managed Ops
// kind. That is a deliberate break with the storage group's convention that a
// kind ending in Ops names one entity: what this kind names is a member, not a
// hub-side entity, and the parameters belong to the kind whose spec it carries.
// One hub kind per managed Ops kind would reintroduce a hub-side copy of a
// managed object for the one category where re-creating an object is dangerous,
// since a re-created StorageNodeOps with the Remove action drains a node twice.
//
// The target block is typed rather than an opaque payload, because this is the
// payload that performs a side effect and so the one that most needs validating
// before it is sent.
//
// StorageDeviceOps is absent from the block. The kind is specified in its design
// and is not declared in the operator's API tree yet, and the block enumerates
// what compiles rather than what is intended. It is added here when that kind
// lands.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fop
// +kubebuilder:printcolumn:name="Member",type=string,JSONPath=".spec.memberRef"
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=".status.target.kind"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Remote",type=string,JSONPath=".status.remote.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.remote.step"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// FleetOperation ships one storage-group Ops object into one member and reports
// what it does there. One kind rather than one per managed Ops kind, because the
// hub's question is which member and which operation, and the parameters belong
// to the kind whose spec it carries.
type FleetOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetOperationSpec   `json:"spec,omitempty"`
	Status FleetOperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FleetOperationList is a list of FleetOperation.
type FleetOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetOperation `json:"items"`
}

// FleetOperationSpec is a request, and every field but Abort is immutable.
type FleetOperationSpec struct {
	// MemberRef names the FleetMember the operation runs in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// Operation is what to run there. Exactly one member is set.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Operation FleetOperationTarget `json:"operation"`

	// Abort asks the member's own object to stop, by setting spec.abort on it
	// through the payload. It is the one mutable field on this spec, and the
	// member's graph decides whether the step it is on accepts it.
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// FleetOperationTarget carries the spec of exactly one storage-group Ops kind.
// The discriminated block is the shape ControlPlane.spec.source uses.
// +kubebuilder:validation:XValidation:rule="[has(self.operatorOps),has(self.storageClusterOps),has(self.storageNodeOps),has(self.storagePoolOps),has(self.storageBackupOps)].filter(x, x).size() == 1",message="set exactly one operation"
type FleetOperationTarget struct {
	// OperatorOps runs an operator-level operation, today a discovery run.
	// +optional
	OperatorOps *storagev1alpha2.OperatorOpsSpec `json:"operatorOps,omitempty"`

	// StorageClusterOps runs a cluster-level operation.
	// +optional
	StorageClusterOps *storagev1alpha2.StorageClusterOpsSpec `json:"storageClusterOps,omitempty"`

	// StorageNodeOps runs a node-level operation.
	// +optional
	StorageNodeOps *storagev1alpha2.StorageNodeOpsSpec `json:"storageNodeOps,omitempty"`

	// StoragePoolOps runs a pool-level operation.
	// +optional
	StoragePoolOps *storagev1alpha2.StoragePoolOpsSpec `json:"storagePoolOps,omitempty"`

	// StorageBackupOps runs a backup or a restore.
	// +optional
	StorageBackupOps *storagev1alpha2.StorageBackupOpsSpec `json:"storageBackupOps,omitempty"`
}

// FleetOperationStatus is the hub's view of an operation running in a member.
type FleetOperationStatus struct {
	// Phase is the hub's own progress, which is about delivery rather than about
	// the operation. What the operation is doing is in Remote.
	// +optional
	Phase FleetOperationPhase `json:"phase,omitempty"`

	// Target names the object the payload created in the member, so that a
	// ManagedClusterView for the whole object needs no re-derivation.
	// +optional
	Target *FleetOperationTargetRef `json:"target,omitempty"`

	// Remote is what the member's own object reports, filled from the status
	// feedback rules on the ManifestWork. Scalars only, which is what a feedback
	// rule carries and what a list view needs. Anything beyond it is a
	// ManagedClusterView against Target.
	// +optional
	Remote *RemoteOpsStatus `json:"remote,omitempty"`

	// Delivery is what the ManifestWork carrying the payload reports.
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the payload was delivered.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation reached a terminal remote phase, and is
	// what retention measures against.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedGeneration is the generation this status was computed from. It
	// advances at most twice, and the second advance is the signal that Abort
	// was observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FleetOperationPhase is the hub's progress at getting the operation to run.
// +kubebuilder:validation:Enum=Pending;Delivering;Running;Succeeded;Failed;Aborted
type FleetOperationPhase string

const (
	// FleetOperationPhasePending means the payload has not been written yet.
	FleetOperationPhasePending FleetOperationPhase = "Pending"
	// FleetOperationPhaseDelivering means the payload is on its way to the member.
	FleetOperationPhaseDelivering FleetOperationPhase = "Delivering"
	// FleetOperationPhaseRunning means the member's own object is working.
	FleetOperationPhaseRunning FleetOperationPhase = "Running"
	// FleetOperationPhaseSucceeded means the member's object reached a terminal success.
	FleetOperationPhaseSucceeded FleetOperationPhase = "Succeeded"
	// FleetOperationPhaseFailed means it reached a terminal failure, or never arrived.
	FleetOperationPhaseFailed FleetOperationPhase = "Failed"
	// FleetOperationPhaseAborted means Abort was observed and the operation stopped.
	FleetOperationPhaseAborted FleetOperationPhase = "Aborted"
)

// FleetOperationTargetRef locates the object the payload created.
type FleetOperationTargetRef struct {
	// Kind is the storage-group kind that was created.
	// +optional
	Kind string `json:"kind,omitempty"`

	// Namespace is where it was created in the member.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the object's name in the member.
	// +optional
	Name string `json:"name,omitempty"`
}

// RemoteOpsStatus is what a feedback rule can carry off a storage-group Ops
// object. Every field is a scalar, because a JSON path feedback rule selects one
// value.
type RemoteOpsStatus struct {
	// Phase is the member object's own phase, in its own spelling.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Step is the state name of the member object's step machine. The deadline
	// beside it in the member is an absolute instant and is deliberately not
	// carried, because it is meaningless against the hub's clock.
	// +optional
	Step string `json:"step,omitempty"`

	// Message is the member object's status message.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the member object's, and is comparable only against
	// generations in the member.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedAt is when the feedback last arrived.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

func init() {
	SchemeBuilder.Register(&FleetOperation{}, &FleetOperationList{})
}
