// A fixture API package for the field-marker generator's tests: one root kind
// whose fields carry the three markers at every depth the walk has to resolve
// (direct, nested, through a pointer, a slice, a map, and an inline embed),
// plus a List kind and a recursive type the walk must not descend forever.

// +groupName=fixture.simplyblock.io
package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// The root marker sits in its own comment block, a blank line above the type's
// doc comment, which is how the kinds in api/ declare theirs.
// +kubebuilder:object:root=true

// Widget is the fixture kind.
type Widget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WidgetSpec   `json:"spec,omitempty"`
	Status WidgetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WidgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Widget `json:"items"`
}

type WidgetSpec struct {
	Shared `json:",inline"`

	// Plain is neither.
	Plain string `json:"plain,omitempty"`

	// Owner is written by the operator.
	// +simplyblock:operatorManaged
	Owner string `json:"owner,omitempty"`

	// Bookkeeping is the operator's own.
	// +simplyblock:invisible
	Bookkeeping string `json:"bookkeeping,omitempty"`

	// Tuning is for the few who need it.
	// +simplyblock:advanced
	Tuning *Detail `json:"tuning,omitempty"`

	Parts  []Part          `json:"parts,omitempty"`
	Detail *Detail         `json:"detail,omitempty"`
	ByName map[string]Part `json:"byName,omitempty"`
	Tree   *Node           `json:"tree,omitempty"`

	// Ignored is not serialized, so it has no path.
	// +simplyblock:invisible
	Ignored string `json:"-"`
}

type Shared struct {
	// Inherited is promoted into the spec by the inline embed.
	// +simplyblock:operatorManaged
	Inherited string `json:"inherited,omitempty"`
}

type Part struct {
	// +simplyblock:operatorManaged
	// +simplyblock:invisible
	Serial string `json:"serial,omitempty"`
}

type Detail struct {
	// +simplyblock:invisible
	Token string `json:"token,omitempty"`
}

type Node struct {
	// +simplyblock:operatorManaged
	Label    string `json:"label,omitempty"`
	Children []Node `json:"children,omitempty"`
}

type WidgetStatus struct {
	// +simplyblock:operatorManaged
	Phase string `json:"phase,omitempty"`
}
