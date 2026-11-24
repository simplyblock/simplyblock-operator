// The tests of the anyuid SCC grant's removal: an operator that once granted it
// deletes the binding it left behind.

package controlplane

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The binding exactly as the earlier operator applied it.
func leftoverAnyUIDBinding(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "simplyblock-scc-anyuid", Namespace: namespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:openshift:scc:anyuid",
		},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: serviceAccountName, Namespace: namespace,
		}},
	}
}

// The control plane pings without sudo, so its account no longer needs anyuid.
// The binding is owned by the ControlPlane and is not garbage-collected while
// the ControlPlane exists, so the operator deletes it on OpenShift.
func TestOnOpenShiftTheLeftoverAnyUIDBindingIsDeleted(t *testing.T) {
	cp := localControlPlane()
	c := newClient(t, cp, leftoverAnyUIDBinding(cp.Namespace))
	r := &ControlPlaneReconciler{Client: c, Scheme: testScheme(t), OpenShift: true}

	if err := r.applyEverything(context.Background(), cp); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var b rbacv1.RoleBinding
	err := c.Get(context.Background(),
		client.ObjectKey{Namespace: cp.Namespace, Name: "simplyblock-scc-anyuid"}, &b)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the anyuid binding is still there (err %v), so the account keeps anyuid", err)
	}
}

// Without a leftover binding the cleanup is a no-op, not an error.
func TestOnOpenShiftNoAnyUIDBindingIsNotAnError(t *testing.T) {
	cp := localControlPlane()
	r := &ControlPlaneReconciler{Client: newClient(t, cp), Scheme: testScheme(t), OpenShift: true}

	if err := r.applyEverything(context.Background(), cp); err != nil {
		t.Fatalf("apply: %v", err)
	}
}
