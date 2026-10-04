// Which of the two ownership mechanisms an object of this deployment gets, and
// the right to delete that the second one grants.
//
// A namespaced object becomes a child of the SimplyblockDriver by controller
// reference and goes with the garbage collector. A cluster-scoped one cannot:
// Kubernetes treats a cluster-scoped object owned by a namespaced one as having
// an owner it cannot resolve and never collects it, so those carry
// storage.simplyblock.io/managed-by instead and the finalizer deletes them.
//
// Specified by operator/docs/designs/crd-redesign/design-simplyblockdriver.md
// §4.1, over the label design-crd-model.md §7.3 defines.

package driver

import (
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// managedByLabel marks an object this operator created without owning. It is
	// the only key with that meaning in the group, so one selector finds
	// everything the operator is responsible for.
	managedByLabel = "storage.simplyblock.io/managed-by"

	// managedByValue is the lowercased kind of the controller that manages the
	// object, rather than the object's name: a label value admits no slash, so a
	// namespace and a name do not fit in one.
	managedByValue = "simplyblockdriver"
)

// setOwnership marks obj as this driver's, by whichever mechanism its scope
// allows. It is additive: adoption meets objects a chart already labeled, and
// dropping those would rewrite a running object for no reason.
func setOwnership(owner client.Object, obj client.Object, scheme *runtime.Scheme) error {
	if obj.GetNamespace() != "" {
		return controllerutil.SetControllerReference(owner, obj, scheme)
	}

	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[managedByLabel] = managedByValue
	obj.SetLabels(labels)
	return nil
}

// mayDelete reports whether this controller marked obj and may therefore remove
// it. A cluster-scoped object is shared ground: one carrying another
// controller's value, or none, belongs to somebody else.
func mayDelete(obj client.Object) bool {
	return obj.GetLabels()[managedByLabel] == managedByValue
}
