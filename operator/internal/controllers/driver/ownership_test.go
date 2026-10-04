// The two ownership mechanisms, and the rule that decides which an object gets.
//
// U-61, U-62, and I-17 in the test plan: namespaced objects are children by
// controller reference, cluster-scoped ones cannot be and carry the managed-by
// label instead, and the label grants a right to delete that stops at another
// controller's value.

package driver

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func ownershipScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := simplyblockv1alpha2.AddToScheme(s); err != nil {
		t.Fatalf("add simplyblock scheme: %v", err)
	}
	return s
}

// U-61: a namespaced object goes with the garbage collector.
func TestNamespacedObjectsBecomeChildren(t *testing.T) {
	d := testDriver("simplyblock")
	s := ownershipScheme(t)

	namespaced := []client.Object{
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: d.Namespace}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: d.Namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sa", Namespace: d.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: d.Namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: d.Namespace}},
	}

	for _, obj := range namespaced {
		if err := setOwnership(d, obj, s); err != nil {
			t.Fatalf("%T: %v", obj, err)
		}
		refs := obj.GetOwnerReferences()
		if len(refs) != 1 || refs[0].Controller == nil || !*refs[0].Controller {
			t.Errorf("%T has no controller reference: %+v", obj, refs)
			continue
		}
		if refs[0].Kind != "SimplyblockDriver" || refs[0].Name != d.Name {
			t.Errorf("%T owner = %s/%s, want SimplyblockDriver/%s", obj, refs[0].Kind, refs[0].Name, d.Name)
		}
		if obj.GetLabels()[managedByLabel] != "" {
			t.Errorf("%T carries the managed-by label as well as an owner reference", obj)
		}
	}
}

// U-62: a cluster-scoped object cannot carry a namespaced owner, because
// Kubernetes treats one as unresolvable and never collects the object.
func TestClusterScopedObjectsCarryTheLabel(t *testing.T) {
	d := testDriver("simplyblock")
	s := ownershipScheme(t)

	clusterScoped := []client.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "r"}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
		&storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "csi.simplyblock.io"}},
	}

	for _, obj := range clusterScoped {
		if err := setOwnership(d, obj, s); err != nil {
			t.Fatalf("%T: %v", obj, err)
		}
		if refs := obj.GetOwnerReferences(); len(refs) != 0 {
			t.Errorf("%T carries an owner reference it cannot be collected through: %+v", obj, refs)
		}
		if got := obj.GetLabels()[managedByLabel]; got != managedByValue {
			t.Errorf("%T managed-by = %q, want %q", obj, got, managedByValue)
		}
	}
}

// I-17: the label grants the right to delete, and it stops at another
// controller's value or at no value at all.
func TestMayDeleteOnlyWhatThisControllerMarked(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		may    bool
	}{
		{"marked by this controller", map[string]string{managedByLabel: managedByValue}, true},
		{"marked by another controller", map[string]string{managedByLabel: "storagepool"}, false},
		{"unmarked", nil, false},
		{"marked under the wrong key", map[string]string{"simplyblock.io/managed-by": managedByValue}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "r", Labels: tc.labels}}
			if got := mayDelete(obj); got != tc.may {
				t.Errorf("mayDelete = %v, want %v", got, tc.may)
			}
		})
	}
}

// Ownership is set without discarding what the object already carries, because
// adoption meets objects a chart labeled.
func TestOwnershipKeepsExistingLabels(t *testing.T) {
	d := testDriver("simplyblock")
	obj := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name:   "r",
		Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"},
	}}

	if err := setOwnership(d, obj, ownershipScheme(t)); err != nil {
		t.Fatalf("setOwnership: %v", err)
	}
	if obj.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		t.Errorf("an existing label was dropped: %+v", obj.Labels)
	}
	if obj.Labels[managedByLabel] != managedByValue {
		t.Errorf("managed-by not set: %+v", obj.Labels)
	}
}
