// StorageClassDeployment: one StorageClass drawing on one pool in one member.
//
// A pool may have zero or more, because nothing about a pool implies a single
// way to consume it: one class with compression on and another with it off, one
// formatted ext4 and one XFS, a permissive ceiling for a batch tenant and a
// tight one beside it.
//
// A namespaced hub kind is also what makes the cluster-scoped object in the
// member delegable. RBAC covers get, update, and delete by resourceNames, but
// never create or list, so no rule can say "may author classes for this pool"
// against StorageClass itself. A pool's owner instead holds verbs on this kind
// in their own namespace, and the work agent writes the cluster-scoped object in
// the member under its own identity.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=scd
// +kubebuilder:printcolumn:name="Member",type=string,JSONPath=".spec.memberRef"
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=".spec.className"
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=".spec.pool.pool"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageClassDeployment is one StorageClass drawing on one pool in one member.
// A pool may have zero or more, because nothing about a pool implies a single
// way to consume it.
type StorageClassDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClassDeploymentSpec   `json:"spec,omitempty"`
	Status StorageClassDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageClassDeploymentList is a list of StorageClassDeployment.
type StorageClassDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageClassDeployment `json:"items"`
}

// StorageClassDeploymentSpec is the class to write in the member, and the pool
// it draws on.
type StorageClassDeploymentSpec struct {
	// MemberRef names the FleetMember the class is written in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// ClassName is the StorageClass name in the member. It is immutable because
	// it is the object's name there, and a rename is a different class.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ClassName string `json:"className"`

	// Pool is the pool this class draws on. The three fields become the three
	// assignment labels on the class, so an author states the pool and never
	// writes a label by hand.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	Pool StorageClassPoolRef `json:"pool"`

	// Template is what the class carries beyond its assignment.
	// +kubebuilder:validation:Required
	Template StorageClassTemplate `json:"template"`
}

// StorageClassPoolRef locates a pool in the member. A class may draw on a pool
// in any namespace, which is why all three parts are stated rather than
// inherited from wherever the class happens to be written.
type StorageClassPoolRef struct {
	// Namespace is the pool's namespace in the member.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Cluster is the StorageCluster the pool belongs to.
	// +kubebuilder:validation:Required
	Cluster string `json:"cluster"`

	// Pool is the StoragePool's name.
	// +kubebuilder:validation:Required
	Pool string `json:"pool"`
}

// StorageClassTemplate is the class body. Everything here is editable, which is
// what makes re-tuning a ceiling an edit rather than a delete and a re-create.
type StorageClassTemplate struct {
	// Parameters are the driver parameters, written in the superseding QoS
	// spelling only, which is what the operator's own generated class does. The
	// three assignment labels are not parameters and are not written here.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// ReclaimPolicy is what happens to a volume when its claim goes.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +optional
	ReclaimPolicy *corev1.PersistentVolumeReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// VolumeBindingMode is when a volume is provisioned.
	// +kubebuilder:validation:Enum=Immediate;WaitForFirstConsumer
	// +optional
	VolumeBindingMode *storagev1.VolumeBindingMode `json:"volumeBindingMode,omitempty"`

	// DisableVolumeExpansion turns off online expansion. The negative spelling
	// is what makes the zero value the default, and every class this product
	// writes today allows expansion.
	// +optional
	DisableVolumeExpansion bool `json:"disableVolumeExpansion,omitempty"`

	// MountOptions are passed to the mount of a volume of this class.
	// +optional
	MountOptions []string `json:"mountOptions,omitempty"`
}

// StorageClassDeploymentStatus is whether the class reached the member.
type StorageClassDeploymentStatus struct {
	// Phase is where the class is.
	// +optional
	Phase StorageClassDeploymentPhase `json:"phase,omitempty"`

	// Delivery is what the ManifestWork carrying this class reports.
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// StorageClassDeploymentPhase is where a class is. PoolMissing is its own value
// because a class naming a pool that is not there is a mistake somebody fixes,
// and not a delivery that failed.
// +kubebuilder:validation:Enum=Delivering;Ready;PoolMissing;Degraded;Failed
type StorageClassDeploymentPhase string

const (
	// StorageClassDeploymentPhaseDelivering means the payload is on its way to the member.
	StorageClassDeploymentPhaseDelivering StorageClassDeploymentPhase = "Delivering"
	// StorageClassDeploymentPhaseReady means the class is present in the member.
	StorageClassDeploymentPhaseReady StorageClassDeploymentPhase = "Ready"
	// StorageClassDeploymentPhasePoolMissing means the pool the class names is not there.
	StorageClassDeploymentPhasePoolMissing StorageClassDeploymentPhase = "PoolMissing"
	// StorageClassDeploymentPhaseDegraded means the class is present, and something about it is not well.
	StorageClassDeploymentPhaseDegraded StorageClassDeploymentPhase = "Degraded"
	// StorageClassDeploymentPhaseFailed means the delivery will not proceed without a change.
	StorageClassDeploymentPhaseFailed StorageClassDeploymentPhase = "Failed"
)

func init() {
	SchemeBuilder.Register(&StorageClassDeployment{}, &StorageClassDeploymentList{})
}
