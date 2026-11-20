// StorageFleet: the control plane a fleet is built on, and the defaults every
// member of it inherits.
//
// It is a singleton named simplyblock, matching the ControlPlane convention in
// the storage group: there is exactly one control plane, and no kind carries a
// reference by which a controller could select among several. It owns nothing,
// because a member outlives an edit to the fleet's defaults and a singleton at
// the root of the ownership tree would make one deletion a fleet-wide cascade.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sf
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.version"
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=".status.memberCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// StorageFleet is the control plane a fleet is built on, and the fleet's
// defaults. One object per hub, named simplyblock.
type StorageFleet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageFleetSpec   `json:"spec,omitempty"`
	Status StorageFleetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageFleetList is a list of StorageFleet.
type StorageFleetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageFleet `json:"items"`
}

// StorageFleetSpec is where the fleet's control plane is, and what its
// deployments inherit.
type StorageFleetSpec struct {
	// ControlPlane is the control plane every member of this fleet is joined to.
	// It is immutable as a block, because re-pointing a live fleet at another
	// control plane produces a different fleet.
	// +kubebuilder:validation:Required
	// +k8s:immutable
	ControlPlane FleetControlPlane `json:"controlPlane"`

	// Defaults are the values a member inherits when it omits them.
	// +optional
	Defaults *FleetDefaults `json:"defaults,omitempty"`
}

// FleetControlPlane addresses the management API. It is the shape
// ControlPlane.spec.source.external takes, because it describes the same
// deployment from the other side.
type FleetControlPlane struct {
	// Endpoint is the management API's base URL.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?(/.*)?$`
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// CredentialsSecretRef names a Secret in this namespace holding the token.
	// +kubebuilder:validation:Required
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// FleetDefaults are fleet-wide values a member inherits.
type FleetDefaults struct {
	// OpsRetention is how long a terminal FleetOperation is kept.
	// +optional
	OpsRetention *metav1.Duration `json:"opsRetention,omitempty"`

	// InventoryRefreshInterval is how often a member is asked to re-probe.
	// +optional
	InventoryRefreshInterval *metav1.Duration `json:"inventoryRefreshInterval,omitempty"`
}

// StorageFleetStatus is what the control plane reports about itself.
type StorageFleetStatus struct {
	// Phase is whether the control plane answers.
	// +optional
	Phase StorageFleetPhase `json:"phase,omitempty"`

	// Endpoint is the resolved base URL, so that a reader asks status not spec.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Version is the management API's reported version.
	// +optional
	Version string `json:"version,omitempty"`

	// MemberCount is how many FleetMember objects this fleet has.
	// +optional
	MemberCount int32 `json:"memberCount,omitempty"`

	// LastChecked is when the readiness probe last ran.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

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

// StorageFleetPhase is whether the fleet's control plane answers. There is no
// Installing value: the control plane exists before the hub does.
// +kubebuilder:validation:Enum=Available;Degraded;Unavailable
type StorageFleetPhase string

const (
	// StorageFleetPhaseAvailable means the management API answers.
	StorageFleetPhaseAvailable StorageFleetPhase = "Available"
	// StorageFleetPhaseDegraded means it answers, and reports a problem of its own.
	StorageFleetPhaseDegraded StorageFleetPhase = "Degraded"
	// StorageFleetPhaseUnavailable means it does not answer.
	StorageFleetPhaseUnavailable StorageFleetPhase = "Unavailable"
)

func init() {
	SchemeBuilder.Register(&StorageFleet{}, &StorageFleetList{})
}
