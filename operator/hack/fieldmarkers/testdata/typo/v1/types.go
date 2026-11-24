// A fixture API package with a misspelled simplyblock marker, which the
// generator must refuse rather than skip: a typo would otherwise leave the
// field unmarked without anyone noticing.

// +groupName=typo.simplyblock.io
package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
type Gadget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GadgetSpec `json:"spec,omitempty"`
}

type GadgetSpec struct {
	// +simplyblock:operatorManged
	Owner string `json:"owner,omitempty"`
}
