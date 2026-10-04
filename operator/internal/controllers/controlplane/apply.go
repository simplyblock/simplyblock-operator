// How the install writes an object, and which of the two ownership mechanisms
// that object gets.
//
// Every write is a server-side apply under a stable field manager. That is what
// makes re-entering a step a no-op (design-controlplane.md §4.2): a step whose
// apply never landed re-applies to the same result, which is why the machine
// carries no triggered flag. It is also what lets the same call take over an
// object a Helm release created, since an apply reconciles field ownership
// rather than failing on a conflict.
//
// A namespaced object becomes a child of the ControlPlane by controller
// reference and goes with the garbage collector when the ControlPlane is
// deleted. A cluster-scoped one cannot: Kubernetes treats a cluster-scoped
// object owned by a namespaced one as having an owner it cannot resolve and
// never collects it. Those carry storage.simplyblock.io/managed-by instead, and
// the finalizer deletes the ones it marked. That is the split
// design-simplyblockdriver.md §4.1 makes, for the same reason.

package controlplane

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// fieldOwner is the field manager every apply here writes under. Taking
	// ownership from a Helm release's manager happens under this name, so it has
	// to stay stable across releases.
	fieldOwner = client.FieldOwner("simplyblock-operator")

	// managedByLabel marks a cluster-scoped object this operator created without
	// owning. It is the one key with that meaning in the group, so a single
	// selector finds everything the operator is responsible for.
	managedByLabel = "storage.simplyblock.io/managed-by"

	// managedByValue is the lowercased kind of the controller that manages the
	// object rather than the object's name, because a label value admits no
	// slash and a namespace and a name do not fit in one.
	managedByValue = "controlplane"
)

// applier writes the install's objects. It is the subset of client.Client the
// workload builders need, named so that the apply path can be exercised without
// a manager.
type applier interface {
	client.Reader
	Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error
}

// applyAll writes every object of a step in order, stopping at the first one
// that fails. The order within a step is the order the objects depend on each
// other: accounts and configuration before the RBAC that names them, and the
// RBAC before the workloads that run under them.
//
// A step is re-entered on every pass through steady state (§4.3), so this is
// also the mechanism that puts back an object somebody deleted and corrects one
// somebody edited.
func applyAll(
	ctx context.Context, c applier, owner client.Object, scheme *runtime.Scheme, objects []client.Object,
) error {
	for _, obj := range objects {
		if err := applyOne(ctx, c, owner, scheme, obj); err != nil {
			return err
		}
	}
	return nil
}

// applyOne marks an object as this control plane's and writes it.
func applyOne(
	ctx context.Context, c applier, owner client.Object, scheme *runtime.Scheme, obj client.Object,
) error {
	if err := setOwnership(owner, obj, scheme); err != nil {
		return fmt.Errorf("set ownership on %s %s: %w", kindOf(obj, scheme), obj.GetName(), err)
	}
	cfg, err := applyConfiguration(obj, scheme)
	if err != nil {
		return fmt.Errorf("encode %s %s for apply: %w", kindOf(obj, scheme), obj.GetName(), err)
	}
	if err := c.Apply(ctx, cfg, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s: %w", kindOf(obj, scheme), obj.GetName(), err)
	}
	return nil
}

// setOwnership marks obj as this control plane's, by whichever mechanism its
// scope allows. It is additive: an install that meets objects a chart already
// labeled leaves those labels in place, because dropping them would rewrite a
// running object for no reason.
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
// controller's value, or none at all, belongs to somebody else and deleting it
// takes away somebody else's RBAC.
func mayDelete(obj client.Object) bool {
	return obj.GetLabels()[managedByLabel] == managedByValue
}

// applyConfiguration turns a built object into the shape a server-side apply
// takes.
//
// The apiVersion and kind have to be on the wire for an apply, and a typed
// object built in Go carries an empty TypeMeta, so the kind is resolved from the
// scheme rather than written by hand at each call site. An unstructured object
// already carries both, which is how the FoundationDB and cert-manager kinds
// this repository has no Go types for are applied.
func applyConfiguration(obj client.Object, scheme *runtime.Scheme) (runtime.ApplyConfiguration, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return client.ApplyConfigurationFromUnstructured(u), nil
	}

	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return nil, err
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	// A built object has no status, and an apply carrying an empty one claims
	// ownership of a field this controller does not set. For a Deployment that
	// means fighting the Deployment controller over its own replica counts.
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return client.ApplyConfigurationFromUnstructured(u), nil
}

// kindOf names an object for an error message. It falls back to the Go type
// where the scheme does not know the kind, since an error about an unregistered
// type must still say which object it was about.
func kindOf(obj client.Object, scheme *runtime.Scheme) string {
	if gvk, err := apiutil.GVKForObject(obj, scheme); err == nil {
		return gvk.Kind
	}
	return fmt.Sprintf("%T", obj)
}

// deleteIfMarked removes a cluster-scoped object the finalizer is responsible
// for, and leaves alone one this controller did not mark. An object that is not
// there is already in the state the caller wants.
func deleteIfMarked(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read %s before deleting it: %w", obj.GetName(), err)
	}
	if !mayDelete(obj) {
		return nil
	}
	if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete %s: %w", obj.GetName(), err)
	}
	return nil
}
