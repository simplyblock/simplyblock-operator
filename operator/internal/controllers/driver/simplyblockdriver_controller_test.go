// U-01, U-03, U-10, U-41, and the controller half of the singleton, U-76 to
// U-80.
//
// The fake client does not implement server-side apply, so these exercise the
// object set, the ownership, the finalizer, and the refusals. That the apply
// takes a field from another manager is the API server's behavior and belongs
// in envtest, which is I-14.

package driver

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func reconcilerScheme(t *testing.T) *runtime.Scheme {
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

func requestFor(d *simplyblockv1alpha2.SimplyblockDriver) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}
}

// The object set is complete: every kind the design lists, and nothing else.
func TestDesiredCoversTheWholeObjectSet(t *testing.T) {
	d := testDriver("simplyblock")
	r := &SimplyblockDriverReconciler{Scheme: reconcilerScheme(t)}

	counts := map[string]int{}
	for _, obj := range r.desired(d) {
		switch obj.(type) {
		case *corev1.ServiceAccount:
			counts["sa"]++
		case *corev1.ConfigMap:
			counts["cm"]++
		case *rbacv1.ClusterRole:
			counts["role"]++
		case *rbacv1.ClusterRoleBinding:
			counts["binding"]++
		case *appsv1.DaemonSet:
			counts["ds"]++
		case *appsv1.StatefulSet:
			counts["sts"]++
		case *storagev1.CSIDriver:
			counts["csidriver"]++
		default:
			counts["other"]++
		}
	}

	want := map[string]int{
		"sa": 2, "cm": 2, "role": 5, "binding": 5,
		"ds": 1, "sts": 1, "csidriver": 1,
		// the VolumeSnapshotClass, which is unstructured
		"other": 1,
	}
	for kind, n := range want {
		if counts[kind] != n {
			t.Errorf("%s count = %d, want %d", kind, counts[kind], n)
		}
	}
}

// The credentials Secret is not in the set. The StorageCluster reconciler
// upserts it with one entry per cluster, and two controllers writing one object
// alternate its contents.
func TestTheCredentialsSecretIsNotOwnedHere(t *testing.T) {
	d := testDriver("simplyblock")
	r := &SimplyblockDriverReconciler{Scheme: reconcilerScheme(t)}

	for _, obj := range r.desired(d) {
		if _, isSecret := obj.(*corev1.Secret); isSecret {
			t.Errorf("the deployment claims Secret %s, which another controller writes", obj.GetName())
		}
	}
}

// U-37: the snapshot class is in the set only when the deployment includes
// snapshot support.
func TestSnapshotClassFollowsTheToggle(t *testing.T) {
	r := &SimplyblockDriverReconciler{Scheme: reconcilerScheme(t)}

	enabled := testDriver("simplyblock")
	disabled := testDriver("simplyblock")
	off := false
	disabled.Spec.EnableVolumeSnapshots = &off

	if len(r.desired(enabled))-len(r.desired(disabled)) != 1 {
		t.Errorf("the toggle changed the object set by %d, want exactly the snapshot class",
			len(r.desired(enabled))-len(r.desired(disabled)))
	}
}

// U-03 and U-62: every object comes out owned, by whichever mechanism its scope
// allows, and none comes out unowned.
func TestEveryAppliedObjectIsOwned(t *testing.T) {
	d := testDriver("simplyblock")
	scheme := reconcilerScheme(t)
	r := &SimplyblockDriverReconciler{Scheme: scheme}

	for _, obj := range r.desired(d) {
		if err := setOwnership(d, obj, scheme); err != nil {
			t.Fatalf("%T %s: %v", obj, obj.GetName(), err)
		}
		owned := len(obj.GetOwnerReferences()) > 0 || mayDelete(obj)
		if !owned {
			t.Errorf("%T %s comes out owned by nobody", obj, obj.GetName())
		}
		if obj.GetNamespace() == "" && len(obj.GetOwnerReferences()) > 0 {
			t.Errorf("cluster-scoped %s carries an owner reference it cannot be collected through", obj.GetName())
		}
	}
}

// U-76 to U-78: an object that is not the oldest applies nothing, holds at
// Installing, and says which object holds the deployment.
func TestASecondDriverAppliesNothing(t *testing.T) {
	scheme := reconcilerScheme(t)

	older := testDriver("simplyblock")
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	younger := testDriver("second")
	younger.CreationTimestamp = metav1.NewTime(time.Now())

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(older, younger).
		WithStatusSubresource(older, younger).
		Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), requestFor(younger)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ds appsv1.DaemonSet
	err := c.Get(context.Background(),
		client.ObjectKey{Namespace: younger.Namespace, Name: names(younger).nodeDaemonSet}, &ds)
	if err == nil {
		t.Error("the second driver applied a DaemonSet")
	}

	var got simplyblockv1alpha2.SimplyblockDriver
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(younger), &got); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Status.Phase != simplyblockv1alpha2.SimplyblockDriverPhaseInstalling {
		t.Errorf("phase = %q, want Installing", got.Status.Phase)
	}
	if got.Status.Message == "" {
		t.Error("the refusal says nothing about which object holds the deployment")
	}
}

// U-79: both callers break a creation-timestamp tie the same way, which is what
// keeps the webhook and the controller from alternating.
func TestATieIsBrokenTheSameWayEveryTime(t *testing.T) {
	at := metav1.NewTime(time.Now())

	a := testDriver("aaa")
	a.Namespace = "ns-b"
	a.CreationTimestamp = at
	b := testDriver("zzz")
	b.Namespace = "ns-a"
	b.CreationTimestamp = at

	first := DeploymentHolder([]simplyblockv1alpha2.SimplyblockDriver{*a, *b})
	second := DeploymentHolder([]simplyblockv1alpha2.SimplyblockDriver{*b, *a})

	if first.Namespace != second.Namespace || first.Name != second.Name {
		t.Fatalf("list order changed the holder: %s/%s against %s/%s",
			first.Namespace, first.Name, second.Namespace, second.Name)
	}
	if first.Namespace != "ns-a" {
		t.Errorf("holder = %s/%s, want the lexicographically first namespace",
			first.Namespace, first.Name)
	}
}

// U-41 and I-17: the finalizer removes the cluster-scoped objects this
// controller marked, leaves one another controller marked, and releases itself
// on every path.
func TestFinalizerRemovesOnlyWhatThisControllerMarked(t *testing.T) {
	scheme := reconcilerScheme(t)

	d := testDriver("simplyblock")
	d.Finalizers = []string{driverFinalizer}
	now := metav1.NewTime(time.Now())
	d.DeletionTimestamp = &now
	n := names(d)

	ours := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name:   n.clusterRole("node"),
		Labels: map[string]string{managedByLabel: managedByValue},
	}}
	theirs := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name:   n.clusterRole("provisioner"),
		Labels: map[string]string{managedByLabel: "storagepool"},
	}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(d, ours, theirs).
		WithStatusSubresource(d).
		Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	if err := r.finalize(context.Background(), d); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	var check rbacv1.ClusterRole
	if err := c.Get(context.Background(), client.ObjectKey{Name: ours.Name}, &check); err == nil {
		t.Errorf("%s survived, and nothing else will remove it", ours.Name)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: theirs.Name}, &check); err != nil {
		t.Errorf("%s was deleted, and it belongs to another controller: %v", theirs.Name, err)
	}
}

// The counts reach status, not only the phase. A phase without them sends a
// reader to kubectl describe to learn which worker is short.
func TestStatusCarriesTheCounts(t *testing.T) {
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	n := names(d)

	node := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: n.nodeDaemonSet, Namespace: d.Namespace},
		Status:     appsv1.DaemonSetStatus{NumberReady: 2, DesiredNumberScheduled: 3},
	}
	controller := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: n.controllerStatefulSet, Namespace: d.Namespace},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	registration := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: n.csiDriver}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(d, node, controller, registration).
		WithStatusSubresource(d).
		Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	h, err := r.observe(context.Background(), d)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if err := r.setHealth(context.Background(), d, h); err != nil {
		t.Fatalf("setHealth: %v", err)
	}

	var got simplyblockv1alpha2.SimplyblockDriver
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), &got); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Status.Phase != simplyblockv1alpha2.SimplyblockDriverPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
	if got.Status.NodesReady != 2 || got.Status.NodesTotal != 3 {
		t.Errorf("counts = %d/%d, want 2/3", got.Status.NodesReady, got.Status.NodesTotal)
	}
	if !got.Status.ControllerReady {
		t.Error("controllerReady is false with a ready replica")
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
}

// A registration that is missing holds the deployment at Installing even with
// both plugins up, because a kubelet that never saw the driver will not ask it
// for anything.
func TestMissingRegistrationHoldsAtInstalling(t *testing.T) {
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	n := names(d)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(d,
			&appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: n.nodeDaemonSet, Namespace: d.Namespace},
				Status:     appsv1.DaemonSetStatus{NumberReady: 3, DesiredNumberScheduled: 3},
			},
			&appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: n.controllerStatefulSet, Namespace: d.Namespace},
				Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
			}).
		WithStatusSubresource(d).
		Build()
	r := &SimplyblockDriverReconciler{Client: c, Scheme: scheme}

	h, err := r.observe(context.Background(), d)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if h.phase != simplyblockv1alpha2.SimplyblockDriverPhaseInstalling {
		t.Errorf("phase = %q, want Installing without a registration", h.phase)
	}
}
