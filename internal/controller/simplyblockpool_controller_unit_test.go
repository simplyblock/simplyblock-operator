package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
)

func TestPoolReconcileAddsFinalizer(t *testing.T) {
	pool := &simplyblockv1alpha1.SimplyBlockPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-a",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockPoolSpec{
			Name:        "p1",
			ClusterName: "cluster-a",
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", "cluster-uuid"),
		testClusterSecret("default", "cluster-a", "cluster-uuid", "secret"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockPool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if !contains(current.Finalizers, "simplyblock.pool.finalizer") {
		t.Fatalf("expected pool finalizer to be added")
	}
}

func TestPoolReconcileDeletionWithoutUUIDDoesNotProgress(t *testing.T) {
	pool := &simplyblockv1alpha1.SimplyBlockPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-b",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.pool.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockPoolSpec{
			Name:        "p1",
			ClusterName: "cluster-a",
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", "cluster-uuid"),
		testClusterSecret("default", "cluster-a", "cluster-uuid", "secret"),
	)
	if err := r.Delete(context.Background(), pool); err != nil {
		t.Fatalf("failed to trigger deletion: %v", err)
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockPool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("failed to get pool: %v", err)
	}
	if !contains(current.Finalizers, "simplyblock.pool.finalizer") {
		t.Fatalf("expected finalizer to remain because deletion requires status.uuid")
	}
}

func TestPoolReconcilePreventsStatusRegressionWhenClusterMissing(t *testing.T) {
	pool := &simplyblockv1alpha1.SimplyBlockPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-c",
			Namespace: "default",
			Finalizers: []string{
				"simplyblock.pool.finalizer",
			},
		},
		Spec: simplyblockv1alpha1.SimplyBlockPoolSpec{
			Name:        "p1",
			ClusterName: "cluster-missing",
		},
		Status: simplyblockv1alpha1.SimplyBlockPoolStatus{
			UUID:   "pool-uuid",
			Status: "online",
		},
	}

	r := newPoolStateTestReconciler(t, pool)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when cluster UUID is unresolved")
	}

	current := &simplyblockv1alpha1.SimplyBlockPool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.UUID != "pool-uuid" {
		t.Fatalf("status UUID regressed unexpectedly: %q", current.Status.UUID)
	}
}

func newPoolStateTestReconciler(t *testing.T, objects ...client.Object) *SimplyBlockPoolReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := simplyblockv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add simplyblock scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.SimplyBlockPool{},
			&simplyblockv1alpha1.SimplyBlockStorageCluster{},
		).
		WithObjects(objects...).
		Build()

	return &SimplyBlockPoolReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
