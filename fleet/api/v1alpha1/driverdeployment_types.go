// DriverDeployment: the CSI driver one member should run.
//
// It is one object per member, so a fleet-wide rollout is a set of them rather
// than one object with a selector. There is no approval field, unlike the
// deployment of a cluster document: a driver version is an edit, not a
// deployment gate.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=dd
// +kubebuilder:printcolumn:name="Member",type=string,JSONPath=".spec.memberRef"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=".spec.driver.image"
// +kubebuilder:printcolumn:name="Running",type=string,JSONPath=".status.runningVersion"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// DriverDeployment is the CSI driver one member should run. It is one object
// per member, so a fleet-wide rollout is a set of them.
type DriverDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DriverDeploymentSpec   `json:"spec,omitempty"`
	Status DriverDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DriverDeploymentList is a list of DriverDeployment.
type DriverDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DriverDeployment `json:"items"`
}

// DriverDeploymentSpec is the driver payload for one member.
type DriverDeploymentSpec struct {
	// MemberRef names the FleetMember this driver is installed in.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	MemberRef string `json:"memberRef"`

	// Driver is the SimplyblockDriver spec that will be applied in the member.
	// +kubebuilder:validation:Required
	Driver storagev1alpha2.SimplyblockDriverSpec `json:"driver"`
}

// DriverDeploymentStatus is what the member's driver reports back.
type DriverDeploymentStatus struct {
	// Phase is where the installation is.
	// +optional
	Phase DriverDeploymentPhase `json:"phase,omitempty"`

	// RunningVersion is what the member's driver reports, fed back from the
	// applied object rather than assumed from the image in the spec.
	// +optional
	RunningVersion string `json:"runningVersion,omitempty"`

	// Delivery is what the ManifestWork carrying this driver reports.
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

// DriverDeploymentPhase is where a driver installation is.
// +kubebuilder:validation:Enum=Delivering;Installing;Ready;Degraded;Failed
type DriverDeploymentPhase string

const (
	// DriverDeploymentPhaseDelivering means the payload is on its way to the member.
	DriverDeploymentPhaseDelivering DriverDeploymentPhase = "Delivering"
	// DriverDeploymentPhaseInstalling means the member's operator is rolling the driver out.
	DriverDeploymentPhaseInstalling DriverDeploymentPhase = "Installing"
	// DriverDeploymentPhaseReady means the driver the spec asked for is serving.
	DriverDeploymentPhaseReady DriverDeploymentPhase = "Ready"
	// DriverDeploymentPhaseDegraded means it is installed, and part of it is not well.
	DriverDeploymentPhaseDegraded DriverDeploymentPhase = "Degraded"
	// DriverDeploymentPhaseFailed means the installation will not proceed without a change.
	DriverDeploymentPhaseFailed DriverDeploymentPhase = "Failed"
)

func init() {
	SchemeBuilder.Register(&DriverDeployment{}, &DriverDeploymentList{})
}
