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
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	testClusterName            = "local-cluster"
	testTargetClusterName      = "cluster-a"
	testTargetClusterUUID      = "cluster-a-uuid"
	apiPathReplicationTargets  = "/api/v2/clusters/" + testClusterUUID + "/replication/targets"
	apiPathReplicationPolicies = "/api/v2/clusters/" + testClusterUUID + "/replication/policies"
)

// newPolicyReconciler creates a ReplicationPolicyReconciler backed by a fake client.
// The spec.policyRef index is registered so MatchingFields queries work.
// Two StorageClusters are pre-populated: local (testClusterName) and target (testTargetClusterName)
// so ResolveClusterUUID succeeds for both spec.clusterName and spec.target.
func newPolicyReconciler(t *testing.T, objects ...client.Object) (*ReplicationPolicyReconciler, client.Client) {
	t.Helper()
	localCluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: "default"},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: testClusterUUID},
	}
	targetCluster := &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testTargetClusterName, Namespace: "default"},
		Status:     simplyblockv1alpha1.StorageClusterStatus{UUID: testTargetClusterUUID},
	}
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	allObjects := append([]client.Object{localCluster, targetCluster}, objects...)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&simplyblockv1alpha1.ReplicationPolicy{}).
		WithObjects(allObjects...).
		WithIndex(&simplyblockv1alpha1.ReplicationPair{}, "spec.policyRef", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationPair).Spec.PolicyRef}
		}).
		Build()
	return &ReplicationPolicyReconciler{Client: cl, Scheme: scheme}, cl
}

func policyRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func getPolicy(t *testing.T, cl client.Client) *simplyblockv1alpha1.ReplicationPolicy {
	t.Helper()
	p := &simplyblockv1alpha1.ReplicationPolicy{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, p); err != nil {
		t.Fatalf("get ReplicationPolicy: %v", err)
	}
	return p
}

// ---------- ignore not-found ----------

func TestPolicy_IgnoreNotFound(t *testing.T) {
	r, _ := newPolicyReconciler(t)
	res, err := r.Reconcile(context.Background(), policyRequest("missing"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}
}

// ---------- finalizer ----------

func TestPolicy_AddsFinalizer(t *testing.T) {
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{ClusterName: testClusterName, Target: "cluster-a"},
	}
	r, cl := newPolicyReconciler(t, policy)
	// No backend called — t.Setenv makes NewClient point somewhere unreachable;
	// the reconciler returns after Update(finalizer) before any API call.
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")
	_, _ = r.Reconcile(context.Background(), policyRequest("pol"))

	got := getPolicy(t, cl)
	if !containsString(got.Finalizers, utils.FinalizerReplicationPolicy) {
		t.Errorf("finalizer not added; finalizers = %v", got.Finalizers)
	}
}

// ---------- ensure backend target (GET + POST) ----------

func TestPolicy_CreatesTargetWhenAbsent(t *testing.T) {
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers: []string{utils.FinalizerReplicationPolicy},
		},
		Spec: simplyblockv1alpha1.ReplicationPolicySpec{ClusterName: testClusterName, Target: "cluster-a"},
	}
	r, cl := newPolicyReconciler(t, policy)

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == apiPathReplicationTargets:
			writeJSON(w, []interface{}{})
		case req.Method == http.MethodPost && req.URL.Path == apiPathReplicationTargets:
			writeJSON(w, map[string]string{"id": "tgt-uuid"})
		case req.Method == http.MethodGet && req.URL.Path == apiPathReplicationPolicies:
			writeJSON(w, []interface{}{})
		case req.Method == http.MethodPost && req.URL.Path == apiPathReplicationPolicies:
			writeJSON(w, map[string]string{"id": "pol-uuid"})
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getPolicy(t, cl)
	if got.Status.BackendTargetID != "tgt-uuid" {
		t.Errorf("BackendTargetID = %q, want tgt-uuid", got.Status.BackendTargetID)
	}
}

func TestPolicy_ReusesExistingTarget(t *testing.T) {
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers: []string{utils.FinalizerReplicationPolicy},
		},
		Spec: simplyblockv1alpha1.ReplicationPolicySpec{ClusterName: testClusterName, Target: "cluster-a"},
	}
	r, cl := newPolicyReconciler(t, policy)

	existingTargets := []interface{}{
		map[string]string{"id": "existing-tgt", "target_cluster_id": testTargetClusterUUID, "target_name": "x"},
	}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == apiPathReplicationTargets:
			writeJSON(w, existingTargets)
		case req.Method == http.MethodPost && req.URL.Path == apiPathReplicationTargets:
			t.Error("POST replication/targets should not be called when target already exists")
			w.WriteHeader(http.StatusInternalServerError)
		case req.Method == http.MethodGet && req.URL.Path == apiPathReplicationPolicies:
			writeJSON(w, []interface{}{})
		case req.Method == http.MethodPost && req.URL.Path == apiPathReplicationPolicies:
			writeJSON(w, map[string]string{"id": "pol-uuid"})
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getPolicy(t, cl)
	if got.Status.BackendTargetID != "existing-tgt" {
		t.Errorf("BackendTargetID = %q, want existing-tgt", got.Status.BackendTargetID)
	}
}

// ---------- mark ready ----------

func TestPolicy_MarksReadyAfterBothIDsPresent(t *testing.T) {
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers: []string{utils.FinalizerReplicationPolicy},
		},
		Spec:   simplyblockv1alpha1.ReplicationPolicySpec{ClusterName: testClusterName, Target: "cluster-a"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{BackendTargetID: "tgt", BackendPolicyID: "bpol"},
	}
	r, cl := newPolicyReconciler(t, policy)
	// Status subresource: set it via UpdateStatus so the fake client tracks it.
	if err := cl.Status().Update(context.Background(), policy); err != nil {
		t.Fatalf("pre-set status: %v", err)
	}

	// No API calls expected — both IDs are already set.
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	_, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getPolicy(t, cl)
	if !got.Status.Ready {
		t.Errorf("status.ready = false, want true")
	}
}

// ---------- deletion ----------

func TestPolicy_DeletionBlockedWhilePairsExist(t *testing.T) {
	now := metav1.Now()
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPolicy},
			DeletionTimestamp: &now,
		},
		Spec:   simplyblockv1alpha1.ReplicationPolicySpec{ClusterName: testClusterName, Target: "cluster-a"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{BackendPolicyID: "bpol", BackendTargetID: "tgt"},
	}
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: "pair1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	r, cl := newPolicyReconciler(t, policy, pair)
	if err := cl.Status().Update(context.Background(), policy); err != nil {
		t.Fatalf("pre-set status: %v", err)
	}
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while pairs exist, got %+v", res)
	}

	// Finalizer must NOT be removed while pair exists.
	got := getPolicy(t, cl)
	if !containsString(got.Finalizers, utils.FinalizerReplicationPolicy) {
		t.Errorf("finalizer was removed prematurely")
	}
}

func TestPolicy_DeletionRemovesBackendAndFinalizer(t *testing.T) {
	now := metav1.Now()
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPolicy},
			DeletionTimestamp: &now,
		},
		Spec:   simplyblockv1alpha1.ReplicationPolicySpec{ClusterName: testClusterName, Target: "cluster-a"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{BackendPolicyID: "bpol", BackendTargetID: "tgt"},
	}
	r, cl := newPolicyReconciler(t, policy)
	if err := cl.Status().Update(context.Background(), policy); err != nil {
		t.Fatalf("pre-set status: %v", err)
	}

	deleted := map[string]bool{}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			deleted[req.URL.Path] = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !deleted["/api/v2/clusters/"+testClusterUUID+"/replication/policies/bpol"] {
		t.Errorf("backend ReplicationPolicy was not deleted")
	}
	if !deleted["/api/v2/clusters/"+testClusterUUID+"/replication/targets/tgt"] {
		t.Errorf("backend ReplicationTarget was not deleted")
	}

	// After removing the last finalizer with DeletionTimestamp set, the fake client GCs the object.
	var gone simplyblockv1alpha1.ReplicationPolicy
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected policy to be GC'd after deletion; err = %v, finalizers = %v", err, gone.Finalizers)
	}
}

// ---------- isTargetOrphaned ----------

func TestPolicy_IsTargetOrphaned(t *testing.T) {
	cases := []struct {
		name     string
		sibling  *simplyblockv1alpha1.ReplicationPolicy
		wantOrph bool
	}{
		{
			name:     "no siblings — orphaned",
			sibling:  nil,
			wantOrph: true,
		},
		{
			name: "sibling with same target — not orphaned",
			sibling: &simplyblockv1alpha1.ReplicationPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "sib", Namespace: "default"},
				Spec:       simplyblockv1alpha1.ReplicationPolicySpec{ClusterName: testClusterName, Target: "cluster-a"},
			},
			wantOrph: false,
		},
		{
			name: "sibling with different target — orphaned",
			sibling: &simplyblockv1alpha1.ReplicationPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "sib", Namespace: "default"},
				Spec:       simplyblockv1alpha1.ReplicationPolicySpec{Target: "cluster-b"},
			},
			wantOrph: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := &simplyblockv1alpha1.ReplicationPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
				Spec:       simplyblockv1alpha1.ReplicationPolicySpec{ClusterName: testClusterName, Target: "cluster-a"},
			}
			objects := []client.Object{policy}
			if tc.sibling != nil {
				objects = append(objects, tc.sibling)
			}
			r, _ := newPolicyReconciler(t, objects...)
			got, err := r.isTargetOrphaned(context.Background(), policy)
			if err != nil {
				t.Fatalf("isTargetOrphaned: %v", err)
			}
			if got != tc.wantOrph {
				t.Errorf("isTargetOrphaned = %v, want %v", got, tc.wantOrph)
			}
		})
	}
}

// ---------- parseDurationToMinutes ----------

func TestParseDurationToMinutes(t *testing.T) {
	cases := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"5m", 5, false},
		{"1h", 60, false},
		{"30s", 1, false}, // clamped to minimum 1
		{"0s", 1, false},  // clamped to minimum 1
		{"90m", 90, false},
		{"invalid", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseDurationToMinutes(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for input %q", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseDurationToMinutes(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
