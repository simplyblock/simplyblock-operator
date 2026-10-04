// ClusterDeployment: the intent to build one storage cluster in one member, the
// document that will be applied there, and the record of it landing.
//
// It is an entity rather than an operation because a deployment is applied
// rather than operated, which is the reading ClusterDeploymentConfig already
// has in the storage group. Its payload is that kind's spec, typed rather than
// opaque, because the document is the thing a reviewer reads and because the
// validation markers on the embedded type are inherited by this CRD: a rule
// expressed in CEL is checked when the document is authored at the hub, where a
// rule expressed in a member's webhook would refuse the write three hops away
// with nowhere to report it.
//
// The document is editable while it is a draft and frozen once approved, which
// is the one place this kind departs from ClusterDeploymentConfig's
// immutability. What ships down is therefore always an already-approved
// document, and the member never holds a draft nobody approved.

package v1alpha1

import (
	"github.com/simplyblock/atlas/statemachine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cd
// +kubebuilder:printcolumn:name="Member",type=string,JSONPath=".spec.memberRef"
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=".spec.approved"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=".status.step.state"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".status.result.storageClusterName"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ClusterDeployment is the intent to build one storage cluster in one member,
// the document that will be applied there, and the record of it landing.
type ClusterDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterDeploymentSpec   `json:"spec,omitempty"`
	Status ClusterDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterDeploymentList is a list of ClusterDeployment.
type ClusterDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterDeployment `json:"items"`
}

// ClusterDeploymentSpec is the document and the approval.
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.config == oldSelf.config",message="config is immutable once approved"
// +kubebuilder:validation:XValidation:rule="!oldSelf.approved || self.approved",message="approval cannot be withdrawn"
type ClusterDeploymentSpec struct {
	// MemberRef names the FleetMember this cluster is built in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// Config is the document that will be applied in the member, composed from
	// that member's inventory. It is a typed storage-group spec rather than an
	// opaque blob because it is the thing a reviewer reads.
	// +kubebuilder:validation:Required
	Config storagev1alpha2.ClusterDeploymentConfigSpec `json:"config"`

	// Approved is the instruction to build, and setting it is the deployment
	// rather than a review stage in front of one.
	// +optional
	Approved bool `json:"approved,omitempty"`

	// ApprovedBy records the identity that approved, which the service account
	// replaying the edit into the member would otherwise erase. A webhook sets
	// it from the request's user info.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`
}

// ClusterDeploymentStatus is how far the build-out got, and what it produced.
type ClusterDeploymentStatus struct {
	// Phase is where the build-out is.
	// +optional
	Phase ClusterDeploymentPhase `json:"phase,omitempty"`

	// Step is the delivery machine's position.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state in ['Composing','Delivering','Applying','Expanding','Verifying']",message="unknown step"
	// +optional
	Step statemachine.KubeSnapshot `json:"step,omitempty"`

	// ConfigFingerprint is a hash of spec.config, and the delivery block's
	// AppliedFingerprint is the one that reached the member. The pair is what
	// makes an edit's arrival answerable without a generation the hub did not
	// issue.
	// +optional
	ConfigFingerprint string `json:"configFingerprint,omitempty"`

	// Delivery is what the ManifestWork carrying this document reports.
	// +optional
	Delivery *DeliveryStatus `json:"delivery,omitempty"`

	// Result names what the member built.
	// +optional
	Result *ClusterDeploymentResult `json:"result,omitempty"`

	// Message is why the phase is what it is.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when delivery began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the build-out reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedGeneration is the generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterDeploymentPhase is where a build-out is. Draft is a phase and not a
// step, because a document waiting for approval is not a delivery that stalled.
// +kubebuilder:validation:Enum=Draft;Delivering;Deploying;Ready;Degraded;Failed
type ClusterDeploymentPhase string

const (
	// ClusterDeploymentPhaseDraft means the document is not approved yet.
	ClusterDeploymentPhaseDraft ClusterDeploymentPhase = "Draft"
	// ClusterDeploymentPhaseDelivering means the payload is on its way to the member.
	ClusterDeploymentPhaseDelivering ClusterDeploymentPhase = "Delivering"
	// ClusterDeploymentPhaseDeploying means the member is expanding the document.
	ClusterDeploymentPhaseDeploying ClusterDeploymentPhase = "Deploying"
	// ClusterDeploymentPhaseReady means the cluster the document asked for is serving.
	ClusterDeploymentPhaseReady ClusterDeploymentPhase = "Ready"
	// ClusterDeploymentPhaseDegraded means it was built, and something in it is not well.
	ClusterDeploymentPhaseDegraded ClusterDeploymentPhase = "Degraded"
	// ClusterDeploymentPhaseFailed means the build-out will not proceed without a change.
	ClusterDeploymentPhaseFailed ClusterDeploymentPhase = "Failed"
)

// ClusterDeploymentStep is one step of the delivery machine.
// +kubebuilder:validation:Enum=Composing;Delivering;Applying;Expanding;Verifying
type ClusterDeploymentStep string

const (
	// ClusterDeploymentStepComposing builds the payload from the member's inventory.
	ClusterDeploymentStepComposing ClusterDeploymentStep = "Composing"
	// ClusterDeploymentStepDelivering writes the ManifestWork.
	ClusterDeploymentStepDelivering ClusterDeploymentStep = "Delivering"
	// ClusterDeploymentStepApplying waits for the work agent to apply it.
	ClusterDeploymentStepApplying ClusterDeploymentStep = "Applying"
	// ClusterDeploymentStepExpanding waits for the member's operator to expand the document.
	ClusterDeploymentStepExpanding ClusterDeploymentStep = "Expanding"
	// ClusterDeploymentStepVerifying reads back what the member built.
	ClusterDeploymentStepVerifying ClusterDeploymentStep = "Verifying"
)

// ClusterDeploymentResult identifies what the member built, so that a console
// reaches it without re-deriving the name.
type ClusterDeploymentResult struct {
	// StorageClusterName is the object's name in the member.
	// +optional
	StorageClusterName string `json:"storageClusterName,omitempty"`

	// StorageClusterUUID is the control plane's identifier, which is what the
	// roll-up and the projection address the same object by.
	// +optional
	StorageClusterUUID string `json:"storageClusterUUID,omitempty"`

	// Namespace is where the objects were created in the member. A payload
	// states its namespace rather than inheriting one, and this records what was
	// used. It bears no relation to the hub-side namespace the intent was
	// written in.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// NodesExpected is how many storage nodes the document asked for.
	// +optional
	NodesExpected int32 `json:"nodesExpected,omitempty"`

	// NodesReady is how many of them answered.
	// +optional
	NodesReady int32 `json:"nodesReady,omitempty"`
}

func init() {
	SchemeBuilder.Register(&ClusterDeployment{}, &ClusterDeploymentList{})
}
