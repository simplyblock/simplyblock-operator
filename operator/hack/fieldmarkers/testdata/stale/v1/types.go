// A fixture API package that does not type-check, the state an API package is
// in after a field is removed and before `make generate` rewrites its deepcopy
// functions (zz_generated.go here). `make manifests` runs first, so the
// generator has to resolve the markers anyway, as controller-gen does.

// +groupName=stale.simplyblock.io
package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true

// Gizmo is the fixture kind.
type Gizmo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GizmoSpec `json:"spec,omitempty"`
}

type GizmoSpec struct {
	// +simplyblock:operatorManaged
	Owner string `json:"owner,omitempty"`
}
