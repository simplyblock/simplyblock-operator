package controller

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-manager/api/v1alpha1"
	webapimock "github.com/simplyblock/simplyblock-manager/internal/webapi/mock"
)

func TestLvolReconcileAddsFinalizer(t *testing.T) {
	lvol := &simplyblockv1alpha1.SimplyBlockLvol{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lvol-a",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.SimplyBlockLvolSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
		},
	}

	r := newLvolStateTestReconciler(t,
		lvol,
		testCluster("default", "cluster-a", "cluster-uuid"),
		testClusterSecret("default", "cluster-a", "cluster-uuid", "secret"),
		testPool("default", "pool-a", "cluster-a", "pool-uuid"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lvol)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lvol), current); err != nil {
		t.Fatalf("failed to get lvol: %v", err)
	}
	if !contains(current.Finalizers, "simplyblock.lvol.finalizer") {
		t.Fatalf("expected lvol finalizer to be added")
	}
}

func TestLvolReconcileDeletionRemovesFinalizer(t *testing.T) {
	lvol := &simplyblockv1alpha1.SimplyBlockLvol{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "lvol-b",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.lvol.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockLvolSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
		},
	}

	r := newLvolStateTestReconciler(t,
		lvol,
		testCluster("default", "cluster-a", "cluster-uuid"),
		testClusterSecret("default", "cluster-a", "cluster-uuid", "secret"),
		testPool("default", "pool-a", "cluster-a", "pool-uuid"),
	)
	if err := r.Delete(context.Background(), lvol); err != nil {
		t.Fatalf("failed to trigger deletion: %v", err)
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lvol)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lvol), current); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("failed to get lvol: %v", err)
	}
	if contains(current.Finalizers, "simplyblock.lvol.finalizer") {
		t.Fatalf("expected lvol finalizer to be removed")
	}
}

func TestLvolReconcilePreventsStatusRegressionWhenPoolMissing(t *testing.T) {
	lvol := &simplyblockv1alpha1.SimplyBlockLvol{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "lvol-c",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.lvol.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockLvolSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-missing",
		},
		Status: simplyblockv1alpha1.SimplyBlockLvolStatus{
			Configured: true,
			Lvols: []simplyblockv1alpha1.LvolStatus{
				{UUID: "lv-1", LvolName: "lv-old"},
			},
		},
	}

	r := newLvolStateTestReconciler(t,
		lvol,
		testCluster("default", "cluster-a", "cluster-uuid"),
		testClusterSecret("default", "cluster-a", "cluster-uuid", "secret"),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lvol)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when pool UUID is unresolved")
	}

	current := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lvol), current); err != nil {
		t.Fatalf("failed to get lvol: %v", err)
	}
	if !current.Status.Configured || len(current.Status.Lvols) != 1 || current.Status.Lvols[0].UUID != "lv-1" {
		t.Fatalf("status regressed unexpectedly: %#v", current.Status)
	}
}

func TestLvolReconcileWorksInNonDefaultNamespace(t *testing.T) {
	const ns = "tenant-c"
	const clusterName = "cluster-c"
	const poolName = "pool-b"
	const clusterUUID = "cluster-uuid-tenant-c"
	const poolUUID = "pool-uuid-tenant-c"

	lvol := &simplyblockv1alpha1.SimplyBlockLvol{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lvol-ns",
			Namespace: ns,
		},
		Spec: simplyblockv1alpha1.SimplyBlockLvolSpec{
			ClusterName: clusterName,
			PoolName:    poolName,
		},
	}

	r := newLvolStateTestReconciler(t,
		lvol,
		testCluster(ns, clusterName, clusterUUID),
		testClusterSecret(ns, clusterName, clusterUUID, "secret"),
		testPool(ns, poolName, clusterName, poolUUID),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lvol)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lvol), current); err != nil {
		t.Fatalf("failed to get lvol: %v", err)
	}
	if current.Namespace != ns {
		t.Fatalf("namespace changed unexpectedly: got %q want %q", current.Namespace, ns)
	}
	if !contains(current.Finalizers, "simplyblock.lvol.finalizer") {
		t.Fatalf("expected lvol finalizer to be added in non-default namespace")
	}
	if current.Spec.ClusterName != clusterName {
		t.Fatalf("clusterName changed unexpectedly: got %q want %q", current.Spec.ClusterName, clusterName)
	}
	if current.Spec.PoolName != poolName {
		t.Fatalf("poolName changed unexpectedly: got %q want %q", current.Spec.PoolName, poolName)
	}
}

func TestLvolReconcileConfiguredAndStatusRefreshViaOpenAPIMock(t *testing.T) {
	const clusterUUID = "cluster-uuid-lvol-sync"
	const poolUUID = "pool-uuid-lvol-sync"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodPut,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID,
		webapimock.RouteResponse{
			Status:  http.StatusOK,
			Body:    `{}`,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	)
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body: `[
				{
					"id":"lv-1",
					"name":"volume-1",
					"nodes":["node-2","node-1"],
					"size":1024,
					"status":"online",
					"pool_name":"pool-a",
					"pool_uuid":"` + poolUUID + `"
				}
			]`,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	lvol := &simplyblockv1alpha1.SimplyBlockLvol{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "lvol-mock",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.lvol.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockLvolSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
		},
	}

	r := newLvolStateTestReconciler(t,
		lvol,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
		testPool("default", "pool-a", "cluster-a", poolUUID),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lvol)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected terminal reconcile without delayed requeue after successful lvol sync, got %+v", res)
	}

	current := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lvol), current); err != nil {
		t.Fatalf("failed to get lvol: %v", err)
	}
	if !current.Status.Configured {
		t.Fatalf("expected configured=true after mocked pool update")
	}
	if len(current.Status.Lvols) != 1 || current.Status.Lvols[0].UUID != "lv-1" {
		t.Fatalf("unexpected lvol status after mocked sync: %#v", current.Status)
	}
	if current.Status.Lvols[0].PoolUUID != poolUUID {
		t.Fatalf("expected lvol pool UUID to match mocked UUID: got %q want %q", current.Status.Lvols[0].PoolUUID, poolUUID)
	}
	reqs := mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected two lvol API calls, got %#v", reqs)
	}
	if reqs[0].Path != "/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID ||
		reqs[1].Path != "/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes" {
		t.Fatalf("expected lvol API calls to use cluster/pool UUIDs, got %#v", reqs)
	}
}

func TestLvolReconcilePoolUpdateNon2xxRequeues(t *testing.T) {
	const clusterUUID = "cluster-uuid-lvol-update-fail"
	const poolUUID = "pool-uuid-lvol-update-fail"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodPut,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID,
		webapimock.RouteResponse{
			Status: http.StatusBadGateway,
			Body:   `{"error":"update failed"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	lvol := &simplyblockv1alpha1.SimplyBlockLvol{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "lvol-mock-update-fail",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.lvol.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockLvolSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
		},
	}

	r := newLvolStateTestReconciler(t,
		lvol,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
		testPool("default", "pool-a", "cluster-a", poolUUID),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lvol)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue after non-2xx pool update, got %+v", res)
	}

	current := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lvol), current); err != nil {
		t.Fatalf("failed to get lvol: %v", err)
	}
	if current.Status.Configured {
		t.Fatalf("expected configured=false after failed pool update")
	}
}

func TestLvolReconcileVolumesFetchNon2xxNoRegression(t *testing.T) {
	const clusterUUID = "cluster-uuid-lvol-fetch-fail"
	const poolUUID = "pool-uuid-lvol-fetch-fail"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes",
		webapimock.RouteResponse{
			Status: http.StatusInternalServerError,
			Body:   `{"error":"fetch failed"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	lvol := &simplyblockv1alpha1.SimplyBlockLvol{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "lvol-mock-fetch-fail",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.lvol.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockLvolSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
		},
		Status: simplyblockv1alpha1.SimplyBlockLvolStatus{
			Configured: true,
		},
	}

	r := newLvolStateTestReconciler(t,
		lvol,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
		testPool("default", "pool-a", "cluster-a", poolUUID),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lvol)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue for volumes fetch non-2xx path, got %+v", res)
	}

	current := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lvol), current); err != nil {
		t.Fatalf("failed to get lvol: %v", err)
	}
	if !current.Status.Configured {
		t.Fatalf("expected configured=true to remain unchanged")
	}
	if len(current.Status.Lvols) != 0 {
		t.Fatalf("expected no lvol entries on failed fetch, got %#v", current.Status.Lvols)
	}
}

func TestLvolReconcileVolumesFetchFailurePreservesExistingLvolStatus(t *testing.T) {
	const clusterUUID = "cluster-uuid-lvol-preserve"
	const poolUUID = "pool-uuid-lvol-preserve"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/"+poolUUID+"/volumes",
		webapimock.RouteResponse{
			Status: http.StatusInternalServerError,
			Body:   `{"error":"fetch failed"}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	lvol := &simplyblockv1alpha1.SimplyBlockLvol{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "lvol-preserve-fail",
			Namespace:  "default",
			Finalizers: []string{"simplyblock.lvol.finalizer"},
		},
		Spec: simplyblockv1alpha1.SimplyBlockLvolSpec{
			ClusterName: "cluster-a",
			PoolName:    "pool-a",
		},
		Status: simplyblockv1alpha1.SimplyBlockLvolStatus{
			Configured: true,
			Lvols: []simplyblockv1alpha1.LvolStatus{
				{UUID: "existing-lvol", LvolName: "existing"},
			},
		},
	}

	r := newLvolStateTestReconciler(t,
		lvol,
		testCluster("default", "cluster-a", clusterUUID),
		testClusterSecret("default", "cluster-a", clusterUUID, "secret"),
		testPool("default", "pool-a", "cluster-a", poolUUID),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lvol)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue after failed volumes fetch")
	}

	current := &simplyblockv1alpha1.SimplyBlockLvol{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(lvol), current); err != nil {
		t.Fatalf("failed to get lvol: %v", err)
	}
	if len(current.Status.Lvols) != 1 || current.Status.Lvols[0].UUID != "existing-lvol" {
		t.Fatalf("expected existing lvol status to be preserved on fetch failure, got %#v", current.Status.Lvols)
	}
}

func newLvolStateTestReconciler(t *testing.T, objects ...client.Object) *SimplyBlockLvolReconciler {
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
			&simplyblockv1alpha1.SimplyBlockLvol{},
			&simplyblockv1alpha1.SimplyBlockPool{},
			&simplyblockv1alpha1.SimplyBlockStorageCluster{},
		).
		WithObjects(objects...).
		Build()

	return &SimplyBlockLvolReconciler{
		Client: cl,
		Scheme: scheme,
	}
}
