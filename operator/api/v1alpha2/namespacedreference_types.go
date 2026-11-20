// References that may name an object in another namespace than the one holding
// them, so that the rule for an omitted namespace is stated once.

package v1alpha2

import "k8s.io/apimachinery/pkg/types"

// NamespacedReference names a namespaced object. An empty namespace means the
// namespace of the object that holds the reference.
type NamespacedReference struct {
	// Name is the object's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the object's namespace. When it is empty, the namespace of the
	// object that holds the reference.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// In returns the object's key, taking defaultNamespace when no namespace is set.
func (r NamespacedReference) In(defaultNamespace string) types.NamespacedName {
	if r.Namespace == "" {
		return types.NamespacedName{Namespace: defaultNamespace, Name: r.Name}
	}
	return types.NamespacedName{Namespace: r.Namespace, Name: r.Name}
}

// NamespacedDnsReference is a NamespacedReference whose name is at most 63
// characters (a DNS label), the bound on a StorageCluster name. controller-gen
// takes the bound from the field marker, so it cannot be a clone of NamespacedReference.
type NamespacedDnsReference struct {
	// Name is the referenced object's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the object's namespace. When it is empty, the namespace of the
	// object that holds the reference.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// In returns the object's key, taking defaultNamespace when no namespace is set.
func (r NamespacedDnsReference) In(defaultNamespace string) types.NamespacedName {
	return NamespacedReference(r).In(defaultNamespace)
}
