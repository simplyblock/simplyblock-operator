// A fixture API package whose operatorManaged marker names a writer the
// generator does not know, which it must refuse: an unknown writer would
// resolve to no identity, and the field would be locked against the writer it
// was meant to admit.

// +groupName=badwriter.simplyblock.io
package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
type Doohickey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DoohickeySpec `json:"spec,omitempty"`
}

type DoohickeySpec struct {
	// +simplyblock:operatorManaged:writers=hbu
	Owner string `json:"owner,omitempty"`
}
