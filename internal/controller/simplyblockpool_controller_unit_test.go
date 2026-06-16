package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	webapimock "github.com/simplyblock/simplyblock-operator/internal/webapi/mock"
)

func TestPoolReconcileAddsFinalizer(t *testing.T) {
	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-a",
			Namespace: "default",
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: "cluster-a",
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", "cluster-uuid"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if !contains(current.Finalizers, utils.FinalizerPool) {
		t.Fatalf("expected pool finalizer to be added")
	}
}

func TestPoolReconcileDeletionWithoutUUIDDoesNotProgress(t *testing.T) {
	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-b",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: "cluster-a",
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", "cluster-uuid"),
	)
	if err := r.Delete(context.Background(), pool); err != nil {
		t.Fatalf("failed to trigger deletion: %v", err)
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("failed to get pool: %v", err)
	}
	if !contains(current.Finalizers, utils.FinalizerPool) {
		t.Fatalf("expected finalizer to remain because deletion requires status.uuid")
	}
}

func TestPoolReconcilePreventsStatusRegressionWhenClusterMissing(t *testing.T) {
	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-c",
			Namespace: "default",
			Finalizers: []string{
				utils.FinalizerPool,
			},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: "cluster-missing",
		},
		Status: simplyblockv1alpha1.PoolStatus{
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

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.UUID != "pool-uuid" {
		t.Fatalf("status UUID regressed unexpectedly: %q", current.Status.UUID)
	}
}

func TestPoolReconcileWorksInNonDefaultNamespace(t *testing.T) {
	const ns = "tenant-b"
	const clusterName = "cluster-b"
	const clusterUUID = "cluster-uuid-tenant-b"

	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-ns",
			Namespace: ns,
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: clusterName,
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster(ns, clusterName, clusterUUID),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Namespace != ns {
		t.Fatalf("namespace changed unexpectedly: got %q want %q", current.Namespace, ns)
	}
	if !contains(current.Finalizers, utils.FinalizerPool) {
		t.Fatalf("expected pool finalizer to be added in non-default namespace")
	}
	if current.Spec.ClusterName != clusterName {
		t.Fatalf("clusterName changed unexpectedly: got %q want %q", current.Spec.ClusterName, clusterName)
	}
}

func TestPoolReconcileStorageClassNameIncludesNamespace(t *testing.T) {
	const clusterName = "simplyblock-cluster-a"
	pool1 := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool1",
			Namespace:  "cluster1",
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: clusterName,
		},
		Status: simplyblockv1alpha1.PoolStatus{
			UUID: "pool-uuid-1",
		},
	}
	pool2 := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool1",
			Namespace:  "cluster2",
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: clusterName,
		},
		Status: simplyblockv1alpha1.PoolStatus{
			UUID: "pool-uuid-2",
		},
	}

	r := newPoolStateTestReconciler(t,
		pool1,
		pool2,
		testCluster("cluster1", clusterName, "cluster-uuid-1"),
		testCluster("cluster2", clusterName, "cluster-uuid-2"),
	)

	for _, pool := range []*simplyblockv1alpha1.Pool{pool1, pool2} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
			t.Fatalf("reconcile %s/%s returned error: %v", pool.Namespace, pool.Name, err)
		}
	}

	for _, tc := range []struct {
		name        string
		clusterUUID string
		namespace   string
	}{
		{
			name:        "simplyblock-cluster1-simplyblock-cluster-a-pool1",
			clusterUUID: "cluster-uuid-1",
			namespace:   "cluster1",
		},
		{
			name:        "simplyblock-cluster2-simplyblock-cluster-a-pool1",
			clusterUUID: "cluster-uuid-2",
			namespace:   "cluster2",
		},
	} {
		sc := &storagev1.StorageClass{}
		if err := r.Get(context.Background(), client.ObjectKey{Name: tc.name}, sc); err != nil {
			t.Fatalf("failed to get StorageClass %q: %v", tc.name, err)
		}
		if sc.Parameters["cluster_id"] != tc.clusterUUID {
			t.Fatalf("StorageClass %q cluster_id = %q, want %q", tc.name, sc.Parameters["cluster_id"], tc.clusterUUID)
		}
		if sc.Labels["storage.simplyblock.io/namespace"] != tc.namespace {
			t.Fatalf("StorageClass %q namespace label = %q, want %q", tc.name, sc.Labels["storage.simplyblock.io/namespace"], tc.namespace)
		}
	}
}

func TestPoolReconcileCreatesPoolViaOpenAPIMock(t *testing.T) {
	const statusOnline = "online"
	const clusterUUID = "cluster-uuid-pool-create"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	// Proactive adoption check: empty list means no existing pool → fall through to POST.
	mock.Register(
		http.MethodGet,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body:   `[]`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

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

	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-mock",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName:          "cluster-a",
			LogicalVolumeMaxSize: "20G",
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", clusterUUID),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected terminal reconcile without delayed requeue after successful pool creation, got %+v", res)
	}

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.UUID != "pool-created" || current.Status.Status != statusOnline {
		t.Fatalf("unexpected status after mocked pool create: %#v", current.Status)
	}
	reqs := mock.Requests()
	if len(reqs) != 2 ||
		reqs[0].Method != http.MethodGet || reqs[0].Path != "/api/v2/clusters/"+clusterUUID+"/storage-pools" ||
		reqs[1].Method != http.MethodPost || reqs[1].Path != "/api/v2/clusters/"+clusterUUID+"/storage-pools" {
		t.Fatalf("expected GET (adoption check) then POST (create) for cluster UUID %q, got %#v", clusterUUID, reqs)
	}
	var body struct {
		VolumeMaxSize int `json:"volume_max_size"`
	}
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatalf("failed to decode pool create request body %q: %v", string(reqs[0].Body), err)
	}
	if body.VolumeMaxSize != 20_000_000_000 {
		t.Fatalf("volume_max_size got %d want %d; body=%s", body.VolumeMaxSize, 20_000_000_000, string(reqs[0].Body))
	}
}

func TestPoolReconcileCreatesPoolViaDTOFormat(t *testing.T) {
	const clusterUUID = "cluster-uuid-pool-create-dto"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()

	mock.Register(
		http.MethodPost,
		"/api/v2/clusters/"+clusterUUID+"/storage-pools/",
		webapimock.RouteResponse{
			Status: http.StatusOK,
			Body: `{
				"id":"pool-created-dto",
				"cluster_id":"` + clusterUUID + `",
				"name":"pool-mock-dto",
				"status":"active",
				"max_size":0,
				"volume_max_size":20000000000,
				"max_rw_iops":100,
				"max_rw_mbytes":200,
				"max_r_mbytes":50,
				"max_w_mbytes":50,
				"capacity":{"date":0,"size_total":0,"size_prov":0,"size_used":0,"size_free":0,"size_util":0},
				"dhchap":false,
				"allowed_hosts":[]
			}`,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
	)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-mock-dto",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: "cluster-a",
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", clusterUUID),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected terminal reconcile without delayed requeue after successful pool creation, got %+v", res)
	}

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.UUID != "pool-created-dto" || current.Status.Status != "active" {
		t.Fatalf("unexpected status after DTO pool create: %#v", current.Status)
	}
}

func TestPoolReconcileCreatePoolNon2xxRequeues(t *testing.T) {
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

	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-mock-fail",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: "cluster-a",
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", clusterUUID),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected delayed requeue after non-2xx pool create, got %+v", res)
	}

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.UUID != "" {
		t.Fatalf("expected UUID to remain empty after failed create, got %q", current.Status.UUID)
	}
}

func TestPoolReconcileDeleteNon2xxKeepsFinalizerAndRequeues(t *testing.T) {
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

	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-delete-fail",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: "cluster-a",
		},
		Status: simplyblockv1alpha1.PoolStatus{
			UUID: poolUUID,
		},
	}

	r := newPoolStateTestReconciler(t,
		pool,
		testCluster("default", "cluster-a", clusterUUID),
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

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if !contains(current.Finalizers, utils.FinalizerPool) {
		t.Fatalf("expected finalizer to remain after failed delete")
	}
}

func newPoolStateTestReconciler(t *testing.T, objects ...client.Object) *PoolReconciler {
	t.Helper()

	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme, storagev1.AddToScheme)
	cl := newTestClient(t, scheme, []client.Object{
		&simplyblockv1alpha1.Pool{},
		&simplyblockv1alpha1.StorageCluster{},
	}, objects...)

	return &PoolReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
}

func TestPoolReconcileRejectsCrossNamespaceClusterReference(t *testing.T) {
	const poolNS = "tenant-a"
	const clusterNS = "tenant-b"
	const clusterName = "shared-cluster"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-cross-ns",
			Namespace:  poolNS,
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: clusterName,
		},
	}

	// StorageCluster exists, but in a different namespace. The Pool controller
	// must refuse to reconcile against it and surface the misconfiguration.
	r := newPoolStateTestReconciler(t,
		pool,
		testCluster(clusterNS, clusterName, "cluster-uuid-tenant-b"),
	)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when cluster reference is invalid, got %+v", res)
	}

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.Status != poolStatusInvalidClusterReference {
		t.Fatalf("pool status.Status = %q, want %q", current.Status.Status, poolStatusInvalidClusterReference)
	}

	rec, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatalf("expected *record.FakeRecorder, got %T", r.Recorder)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, poolEventInvalidClusterReference) {
			t.Fatalf("event %q did not contain reason %q", ev, poolEventInvalidClusterReference)
		}
	default:
		t.Fatalf("expected an InvalidClusterReference event")
	}

	if reqs := mock.Requests(); len(reqs) != 0 {
		t.Fatalf("expected no webapi calls, got %d: %+v", len(reqs), reqs)
	}
}

func TestPoolReconcileDoesNotEmitEventWhenClusterUUIDNotReady(t *testing.T) {
	const ns = "tenant-c"
	const clusterName = "cluster-pending"

	mock := webapimock.NewSpecServerFromFile(t, "../../openapi.json", false)
	defer mock.Close()
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", mock.URL())

	pool := &simplyblockv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool-pending-cluster",
			Namespace:  ns,
			Finalizers: []string{utils.FinalizerPool},
		},
		Spec: simplyblockv1alpha1.PoolSpec{
			ClusterName: clusterName,
		},
	}

	// Cluster exists in the same namespace but its UUID has not been published yet.
	// This is a normal transient state and must not look the same as a misconfigured
	// cross-namespace reference.
	clusterUUIDPending := testCluster(ns, clusterName, "")

	r := newPoolStateTestReconciler(t, pool, clusterUUIDPending)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue while waiting for cluster UUID, got %+v", res)
	}

	current := &simplyblockv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), current); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if current.Status.Status == poolStatusInvalidClusterReference {
		t.Fatalf("pool status.Status must not be %q while cluster UUID is merely pending", poolStatusInvalidClusterReference)
	}

	rec := r.Recorder.(*record.FakeRecorder)
	select {
	case ev := <-rec.Events:
		t.Fatalf("did not expect an event, got %q", ev)
	default:
	}

	if reqs := mock.Requests(); len(reqs) != 0 {
		t.Fatalf("expected no webapi calls while UUID pending, got %d: %+v", len(reqs), reqs)
	}
}
