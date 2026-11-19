// Removal of what an earlier install of a relocatable add-on left in another
// namespace. A Kubernetes cluster runs one ControlPlane, so an install in a
// namespace other than the ControlPlane's belongs to one that was deleted, and
// re-installing beside the new one would otherwise leave its workload behind.

package prerequisites

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const kindClusterRoleBinding = "ClusterRoleBinding"

// previousNamespace is the namespace an earlier install of the add-on runs in,
// read from the ServiceAccount subject of the ClusterRoleBinding it installed.
// The binding is cluster-scoped, so it names exactly one install. Empty when
// the binding is absent or is not the operator's.
func (p Prerequisite) previousNamespace(ctx context.Context, c client.Reader, objs []*unstructured.Unstructured) (string, error) {
	for _, o := range objs {
		if o.GetKind() != kindClusterRoleBinding {
			continue
		}
		var crb rbacv1.ClusterRoleBinding
		err := c.Get(ctx, client.ObjectKey{Name: o.GetName()}, &crb)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return "", fmt.Errorf("reading ClusterRoleBinding %s: %w", o.GetName(), err)
		case crb.Labels[LabelInstalledBy] != p.Name:
			continue
		}
		for _, s := range crb.Subjects {
			if s.Kind == rbacv1.ServiceAccountKind {
				return s.Namespace, nil
			}
		}
	}
	return "", nil
}

// pruneRelocated deletes the namespaced objects an earlier install left in a
// namespace other than target. Only objects carrying the operator's label are
// deleted: one of the same name without it is somebody else's. The
// cluster-scoped objects are left to the install, which re-applies them.
func (p Prerequisite) pruneRelocated(ctx context.Context, c client.Client, objs []*unstructured.Unstructured, target string) error {
	previous, err := p.previousNamespace(ctx, c, objs)
	if err != nil || previous == "" || previous == target {
		return err
	}
	for _, o := range objs {
		if o.GetNamespace() == "" {
			continue
		}
		old := &unstructured.Unstructured{}
		old.SetGroupVersionKind(o.GroupVersionKind())
		err := c.Get(ctx, client.ObjectKey{Namespace: previous, Name: o.GetName()}, old)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return fmt.Errorf("reading %s %s/%s: %w", o.GetKind(), previous, o.GetName(), err)
		case old.GetLabels()[LabelInstalledBy] != p.Name:
			continue
		}
		err = c.Delete(ctx, old, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("removing %s %s/%s left by an earlier install: %w", o.GetKind(), previous, o.GetName(), err)
		}
	}
	return nil
}
