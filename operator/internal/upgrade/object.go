// How the framework names an object it found, reports on, or intends to change.
// A reference here is a value rather than a pointer to a live object, because a
// finding outlives the read that produced it and a plan is rendered long after
// the cluster was walked.

package upgrade

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ObjectRef identifies one Kubernetes object across a whole migration: the kind
// it is, where it lives, and the identity it was read at.
type ObjectRef struct {
	// GVK is the group, version, and kind. The version is the one the object
	// was read at, which matters here: the same object read at v1alpha1 and at
	// v1alpha2 is two representations of one thing.
	GVK schema.GroupVersionKind

	// Namespace is empty for a cluster-scoped object.
	Namespace string

	// Name is the object's metadata.name.
	Name string

	// UID is what proves an object was adopted rather than rebuilt. A new UID
	// on an object that was supposed to survive is a failure of the release
	// handover, not a change to accept.
	UID types.UID
}

// RefOf builds a reference to a live object. The kind is taken from the object
// rather than from the scheme, so a partially typed object read as unstructured
// references correctly.
func RefOf(obj client.Object) ObjectRef {
	return ObjectRef{
		GVK:       obj.GetObjectKind().GroupVersionKind(),
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		UID:       obj.GetUID(),
	}
}

// Key is the reference without its kind or identity, for a client lookup.
func (r ObjectRef) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: r.Namespace, Name: r.Name}
}

// String renders the reference the way the reports do: Kind namespace/name for
// a namespaced object, and Kind name for a cluster-scoped one.
func (r ObjectRef) String() string {
	kind := r.GVK.Kind
	if kind == "" {
		kind = "Object"
	}
	if r.Namespace == "" {
		return fmt.Sprintf("%s %s", kind, r.Name)
	}
	return fmt.Sprintf("%s %s/%s", kind, r.Namespace, r.Name)
}

// Identity is the reference reduced to what makes two reads the same object,
// dropping the version so that one object read at two API versions compares
// equal. It is the map key the graph and the collision checks use.
func (r ObjectRef) Identity() ObjectIdentity {
	return ObjectIdentity{
		Group:     r.GVK.Group,
		Kind:      r.GVK.Kind,
		Namespace: r.Namespace,
		Name:      r.Name,
	}
}

// ObjectIdentity is a comparable object identity with the API version left out.
type ObjectIdentity struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}
