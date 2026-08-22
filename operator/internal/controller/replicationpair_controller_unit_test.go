package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/ctrltest"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// newSitePairReconciler creates a ReplicationPairReconciler backed by a fake client.
// It indexes ReplicationPolicy.spec.pairRef so deletion-blocking checks work.
func newSitePairReconciler(t *testing.T, objects ...client.Object) (*ReplicationPairReconciler, client.Client) {
	t.Helper()
	scheme := ctrltest.NewScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.ReplicationPair{},
			&simplyblockv1alpha1.ReplicationPolicy{},
		).
		WithObjects(objects...).
		WithIndex(&simplyblockv1alpha1.ReplicationPolicy{}, "spec.pairRef", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationPolicy).Spec.PairRef}
		}).
		Build()
	return &ReplicationPairReconciler{
		Client: cl,
		Scheme: scheme,
	}, cl
}

func sitePairRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func getSitePair(t *testing.T, cl client.Client) *simplyblockv1alpha1.ReplicationPair {
	t.Helper()
	p := &simplyblockv1alpha1.ReplicationPair{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pair1"}, p); err != nil {
		t.Fatalf("get ReplicationPair: %v", err)
	}
	return p
}

func newSitePair() *simplyblockv1alpha1.ReplicationPair {
	return &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pair1",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerReplicationPair},
		},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			SourceCluster: "cluster1",
			TargetCluster: "cluster2",
		},
	}
}

// ---------- ignore not-found ----------

func TestSitePair_IgnoreNotFound(t *testing.T) {
	r, _ := newSitePairReconciler(t)
	res, err := r.Reconcile(context.Background(), sitePairRequest("missing"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

// ---------- finalizer added on first reconcile ----------

func TestSitePair_AddsFinalizer(t *testing.T) {
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: "pair1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			SourceCluster: "cluster1",
			TargetCluster: "cluster2",
		},
	}
	r, cl := newSitePairReconciler(t, pair)
	_, _ = r.Reconcile(context.Background(), sitePairRequest("pair1"))

	got := getSitePair(t, cl)
	if !contains(got.Finalizers, utils.FinalizerReplicationPair) {
		t.Errorf("finalizer not added; finalizers = %v", got.Finalizers)
	}
}

// ---------- cluster UUID not resolvable → waits ----------

func TestSitePair_ClusterUUIDNotReady_Waits(t *testing.T) {
	// No StorageCluster in the fake client → ResolveClusterUUID returns error.
	pair := newSitePair()
	r, _ := newSitePairReconciler(t, pair)

	res, err := r.Reconcile(context.Background(), sitePairRequest("pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when cluster UUID not resolvable")
	}
}

// ---------- backend target created → pair becomes ready ----------

func TestSitePair_CreatesBackendTarget(t *testing.T) {
	// Clusters are pre-seeded with UUIDs via WithObjects; status is read directly by Get.
	cluster1 := testCluster("default", "cluster1", "src-uuid")
	cluster2 := testCluster("default", "cluster2", "tgt-uuid")
	pair := newSitePair()

	// API: GET targets returns empty list; POST creates target with ID.
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if req.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			resp, _ := json.Marshal(map[string]string{"id": "tgt-backend-uuid"})
			_, _ = w.Write(resp)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSitePairReconciler(t, cluster1, cluster2, pair)

	res, err := r.Reconcile(context.Background(), sitePairRequest("pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replPairSyncInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replPairSyncInterval)
	}

	got := getSitePair(t, cl)
	if !got.Status.Ready {
		t.Errorf("pair.Status.Ready = false, want true")
	}
	if got.Status.BackendTargetID != "tgt-backend-uuid" {
		t.Errorf("BackendTargetID = %q, want tgt-backend-uuid", got.Status.BackendTargetID)
	}
}

// ---------- backend target already exists → reuses it ----------

func TestSitePair_ReuseExistingTarget(t *testing.T) {
	cluster1 := testCluster("default", "cluster1", "src-uuid")
	cluster2 := testCluster("default", "cluster2", "tgt-uuid")
	pair := newSitePair()

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			resp, _ := json.Marshal([]map[string]string{
				{"id": "existing-tgt-id", "target_cluster_id": "tgt-uuid"},
			})
			_, _ = w.Write(resp)
			return
		}
		// POST must NOT be called; fail if it is.
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSitePairReconciler(t, cluster1, cluster2, pair)

	_, err := r.Reconcile(context.Background(), sitePairRequest("pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getSitePair(t, cl)
	if got.Status.BackendTargetID != "existing-tgt-id" {
		t.Errorf("BackendTargetID = %q, want existing-tgt-id", got.Status.BackendTargetID)
	}
}

// ---------- deletion blocked while policy references pair ----------

func TestSitePair_DeletionBlockedByPolicy(t *testing.T) {
	now := metav1.Now()
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pair1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPair},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{SourceCluster: "cluster1", TargetCluster: "cluster2"},
	}
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
	}

	r, cl := newSitePairReconciler(t, pair, policy)

	res, err := r.Reconcile(context.Background(), sitePairRequest("pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while policy references pair")
	}

	got := getSitePair(t, cl)
	if !contains(got.Finalizers, utils.FinalizerReplicationPair) {
		t.Errorf("finalizer was removed while policy still references pair")
	}
}

// ---------- deletion succeeds when no policies reference pair ----------

func TestSitePair_DeletionSucceeds(t *testing.T) {
	now := metav1.Now()
	// Status is pre-set in the struct; fake client stores it even with WithStatusSubresource.
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pair1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPair},
			DeletionTimestamp: &now,
		},
		Spec:   simplyblockv1alpha1.ReplicationPairSpec{SourceCluster: "cluster1", TargetCluster: "cluster2"},
		Status: simplyblockv1alpha1.ReplicationPairStatus{BackendTargetID: "tgt-id"},
	}
	cluster1 := testCluster("default", "cluster1", "src-uuid")

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSitePairReconciler(t, pair, cluster1)

	_, err := r.Reconcile(context.Background(), sitePairRequest("pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gone simplyblockv1alpha1.ReplicationPair
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pair1"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected pair to be GC'd after deletion; err = %v, finalizers = %v", err, gone.Finalizers)
	}
}
