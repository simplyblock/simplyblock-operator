// ControlPlane in the shape design-controlplane.md settles: the image the
// operator installs moves under spec.source.managed, and the readiness phase says
// Available rather than Ready.
//
// Only the renamed and regrouped properties are here. spec.source.external
// (design-controlplane.md §5.2) and the rest of ManagedControlPlane are additive
// design work rather than renames, so they are not in this package yet; the
// source struct exists because the regrouping needs somewhere to put the image.

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ManagedControlPlane is a control plane the operator installs and owns.
type ManagedControlPlane struct {
	// Image is the container image used for the simplyblock control-plane
	// workloads (e.g., quay.io/simplyblock-io/simplyblock:26.2.2).
	// Must reference one of the trusted registries (`quay.io/simplyblock-io`, `docker.io/simplyblock`, `public.ecr.aws/simply-block`); digest pinning (@sha256:...) is recommended.
	// +optional
	// +kubebuilder:validation:Pattern=`^($|(quay\.io/simplyblock-io|docker\.io/simplyblock|public\.ecr\.aws/simply-block)/[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*(@sha256:[a-f0-9]{64})?)$`
	Image string `json:"image,omitempty"`
}

// ControlPlaneSource selects where the control plane comes from. Today, it names
// only a managed one, which is what the operator installs.
type ControlPlaneSource struct {
	// Managed is the control plane the operator installs and owns.
	// +optional
	Managed *ManagedControlPlane `json:"managed,omitempty"`
}

// ControlPlaneSpec holds configuration for the singleton ControlPlane resource.
type ControlPlaneSpec struct {
	// Source says where the control plane comes from. It replaces the top-level
	// image field of v1alpha1, which conflated the control plane's own image with
	// the default every StorageNodeSet inherited.
	// +optional
	Source *ControlPlaneSource `json:"source,omitempty"`
}

// ControlPlaneStatus reflects the observed readiness of the simplyblock
// control plane (FDB + management API).
type ControlPlaneStatus struct {
	// Phase is Initializing while the control plane is not yet healthy,
	// and Available once the FDB health check passes.
	// +kubebuilder:validation:Enum=Initializing;Available
	Phase string `json:"phase,omitempty"`

	// Message contains a human-readable explanation of the current phase,
	// for example, the FDB error returned by the health endpoint.
	Message string `json:"message,omitempty"`

	// LastChecked is the timestamp of the most recent FDB health probe.
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`
}

// v1alpha2 is the storage version in the manifests this repository ships, which
// are the ones a fresh install applies. A cluster installed today stores this
// shape from the first write and never converts anything, so the conversion
// webhook is inert there and is not deployed.
//
// An upgrade of an existing cluster is the other path, and it does not take this
// value. The upgrade tool applies these same CRDs with storage held at v1alpha1,
// because a server-side apply overwrites the live storage version and moving it
// before the conversion webhook is serving breaks every write. It flips to
// v1alpha2 with the storage rewrite once the migration has run
// (design-api-upgrade.md §24, design-property-renames.md §3.8).
// +kubebuilder:storageversion
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Initializing while FDB is not ready; Available once the control plane is operational"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.message",description="Human-readable status detail"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ControlPlane is a singleton resource (one per namespace, named "simplyblock")
// that reflects the readiness of the simplyblock control plane. It is created
// automatically by the Helm chart and should not be created or deleted manually.
type ControlPlane struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec ControlPlaneSpec `json:"spec,omitempty"`

	// +optional
	Status ControlPlaneStatus `json:"status,omitempty"`
}

// Hub marks this version as the conversion hub for ControlPlane. It carries no
// behavior: its presence is what tells controller-runtime which version every
// other one converts through.
func (*ControlPlane) Hub() {}

// +kubebuilder:object:root=true

// ControlPlaneList contains a list of ControlPlane resources.
type ControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ControlPlane{}, &ControlPlaneList{})
}
