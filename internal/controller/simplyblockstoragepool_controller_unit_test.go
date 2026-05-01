package controller

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

func TestStoragePoolReconcileAddsFinalizer(t *testing.T) {
	pool := &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-a",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.StoragePoolSpec{
			Name:        "p1",
			ClusterName: "cluster-a",
		},
	}

	r := newStoragePoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", "cluster-uuid"),
		testClusterSecret("default", "cluster-a", "cluster-uuid", "secret"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.StoragePool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if !contains(current.Finalizers, utils.FinalizerStoragePool) {
		t.Fatalf("expected pool finalizer to be added")
	}
}

func TestStoragePoolReconcileDeletionWithoutUUIDDoesNotProgress(t *testing.T) {
	pool := &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-b",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerStoragePool},
		},
		Spec: simplyblockv1alpha1.StoragePoolSpec{
			Name:        "p1",
			ClusterName: "cluster-a",
		},
	}

	r := newStoragePoolStateTestReconciler(t,
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

	current := &simplyblockv1alpha1.StoragePool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("failed to get pool: %v", err)
	}
	if !contains(current.Finalizers, utils.FinalizerStoragePool) {
		t.Fatalf("expected finalizer to remain because deletion requires status.uuid")
	}
}

func TestStoragePoolReconcilePreventsStatusRegressionWhenClusterMissing(t *testing.T) {
	pool := &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-c",
			Namespace: "default",
			Finalizers: []string{
				utils.FinalizerStoragePool,
			},
		},
		Spec: simplyblockv1alpha1.StoragePoolSpec{
			Name:        "p1",
			ClusterName: "cluster-missing",
		},
		Status: simplyblockv1alpha1.StoragePoolStatus{
			UUID:   "pool-uuid",
			Status: "online",
		},
	}

	r := newStoragePoolStateTestReconciler(t, pool)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when cluster UUID is unresolved")
	}

	current := &simplyblockv1alpha1.StoragePool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.UUID != "pool-uuid" {
		t.Fatalf("status UUID regressed unexpectedly: %q", current.Status.UUID)
	}
}

func TestStoragePoolReconcileWorksInNonDefaultNamespace(t *testing.T) {
	const ns = "tenant-b"
	const clusterName = "cluster-b"
	const clusterUUID = "cluster-uuid-tenant-b"

	pool := &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-ns",
			Namespace: ns,
		},
		Spec: simplyblockv1alpha1.StoragePoolSpec{
			Name:        "p1",
			ClusterName: clusterName,
		},
	}

	r := newStoragePoolStateTestReconciler(t,
		pool,
		testCluster(ns, clusterName, clusterUUID),
		testClusterSecret(ns, clusterName, clusterUUID, "secret"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.StoragePool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Namespace != ns {
		t.Fatalf("namespace changed unexpectedly: got %q want %q", current.Namespace, ns)
	}
	if !contains(current.Finalizers, utils.FinalizerStoragePool) {
		t.Fatalf("expected pool finalizer to be added in non-default namespace")
	}
	if current.Spec.ClusterName != clusterName {
		t.Fatalf("clusterName changed unexpectedly: got %q want %q", current.Spec.ClusterName, clusterName)
	}
}

func TestStoragePoolReconcileCreatesPoolViaOpenAPIMock(t *testing.T) {
	const statusOnline = "online"
	const clusterUUID = "cluster-uuid-pool-create"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodPost,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body: `{
				"uuid":"pool-created",
				"status":"online",
				"max_rw_ios_per_sec":100,
				"max_rw_mbytes_per_sec":200,
				"max_r_mbytes_per_sec":50,
				"max_w_mbytes_per_sec":50,
				"qos_host":"qos-node-1"
			}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	pool := &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-mock",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerStoragePool},
		},
		Spec: simplyblockv1alpha1.StoragePoolSpec{
			Name:        "p1",
			ClusterName: "cluster-a",
		},
	}

	r := newStoragePoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected terminal reconcile without delayed requeue after successful pool creation, got %+v", res)
	}

	current := &simplyblockv1alpha1.StoragePool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.UUID != "pool-created" || current.Status.Status != statusOnline {
		t.Fatalf("unexpected status after mocked pool create: %#v", current.Status)
	}
	reqs := mock.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/api/v2/clusters/"+clusterUUID+"/storage-pools" {
		t.Fatalf("expected pool API call with cluster UUID %q, got %#v", clusterUUID, reqs)
	}
}

func TestStoragePoolReconcileCreatePoolNon2xxRequeues(t *testing.T) {
	const clusterUUID = "cluster-uuid-pool-create-fail"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodPost,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/",
		webapimock.RouteResponse{
			Status: http.StatusBadGateway,
			Body:   `{"error":"pool create failed"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	pool := &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-mock-fail",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerStoragePool},
		},
		Spec: simplyblockv1alpha1.StoragePoolSpec{
			Name:        "p1",
			ClusterName: "cluster-a",
		},
	}

	r := newStoragePoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue after non-2xx pool create, got %+v", res)
	}

	current := &simplyblockv1alpha1.StoragePool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.UUID != "" {
		t.Fatalf("expected UUID to remain empty after failed create, got %q", current.Status.UUID)
	}
}

func TestStoragePoolReconcileDeleteNon2xxKeepsFinalizerAndRequeues(t *testing.T) {
	const clusterUUID = "cluster-uuid-pool-delete-fail"
	const poolUUID = "pool-uuid-delete-fail"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodDelete,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID,
		webapimock.RouteResponse{
			Status: http.StatusInternalServerError,
			Body:   `{"error":"delete failed"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	pool := &simplyblockv1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-delete-fail",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerStoragePool},
		},
		Spec: simplyblockv1alpha1.StoragePoolSpec{
			Name:        "p1",
			ClusterName: "cluster-a",
		},
		Status: simplyblockv1alpha1.StoragePoolStatus{
			UUID: poolUUID,
		},
	}

	r := newStoragePoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
	)
	if err := r.Delete(context.Background(), pool); err != nil {
		t.Fatalf("failed to trigger deletion: %v", err)
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue after non-2xx pool delete, got %+v", res)
	}

	current := &simplyblockv1alpha1.StoragePool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if !contains(current.Finalizers, utils.FinalizerStoragePool) {
		t.Fatalf("expected finalizer to remain after failed delete")
	}
}

func newStoragePoolStateTestReconciler(t *testing.T, objects ...client.Object) *StoragePoolReconciler {
	t.Helper()

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.StoragePool{},
		&simplyblockv1alpha1.StorageCluster{},
	}, objects...)

	return &StoragePoolReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
