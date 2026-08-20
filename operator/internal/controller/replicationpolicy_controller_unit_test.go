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
	apiPathReplicationPolicies = "/api/v2/clusters/" + testClusterUUID + "/replication/policies"
)

// newPolicyReconciler creates a ReplicationPolicyReconciler backed by a fake client.
// A StorageCluster (testClusterName → testClusterUUID) is pre-populated.
// ReplicationSlots are indexed by spec.policyRef for slot-count and deletion-block queries.
func newPolicyReconciler(t *testing.T, objects ...client.Object) (*ReplicationPolicyReconciler, client.Client) {
	t.Helper()
	localCluster := testCluster("default", testClusterName, testClusterUUID)
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	allObjects := append([]client.Object{localCluster}, objects...)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		// ReplicationPolicy status is tracked as a subresource; pairs and slots are not,
		// so their full structs (including status) are preserved by WithObjects.
		WithStatusSubresource(&simplyblockv1alpha1.ReplicationPolicy{}).
		WithObjects(allObjects...).
		WithIndex(&simplyblockv1alpha1.ReplicationSlot{}, "spec.policyRef", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationSlot).Spec.PolicyRef}
		}).
		Build()
	return &ReplicationPolicyReconciler{Client: cl, Scheme: scheme}, cl
}

// readyPairForPolicy returns a ReplicationPair that is ready and has a backend target ID.
func readyPairForPolicy(name, sourceCluster, backendTargetID string) *simplyblockv1alpha1.ReplicationPair {
	return &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			SourceCluster: sourceCluster,
			TargetCluster: "cluster-b",
		},
		Status: simplyblockv1alpha1.ReplicationPairStatus{
			Ready:           true,
			BackendTargetID: backendTargetID,
		},
	}
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

// ---------- pair not found → waits ----------

func TestPolicy_PairNotFound_Waits(t *testing.T) {
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "nonexistent-pair"},
	}
	r, _ := newPolicyReconciler(t, policy)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when pair not found")
	}
}

// ---------- pair not ready → waits ----------

func TestPolicy_PairNotReady_Waits(t *testing.T) {
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: "pair1", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPairSpec{SourceCluster: testClusterName},
		Status:     simplyblockv1alpha1.ReplicationPairStatus{Ready: false},
	}
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
	}
	r, _ := newPolicyReconciler(t, pair, policy)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when pair not ready")
	}
}

// ---------- finalizer ----------

func TestPolicy_AddsFinalizer(t *testing.T) {
	pair := readyPairForPolicy("pair1", testClusterName, "tgt-uuid")
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
	}
	r, cl := newPolicyReconciler(t, pair, policy)
	// Backend unreachable — reconciler adds finalizer and returns before API call.
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")
	_, _ = r.Reconcile(context.Background(), policyRequest("pol"))

	got := getPolicy(t, cl)
	if !contains(got.Finalizers, utils.FinalizerReplicationPolicy) {
		t.Errorf("finalizer not added; finalizers = %v", got.Finalizers)
	}
}

// ---------- ensure backend policy (GET + POST) ----------

func TestPolicy_CreatesBackendPolicy_WhenAbsent(t *testing.T) {
	pair := readyPairForPolicy("pair1", testClusterName, "tgt-uuid")
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers: []string{utils.FinalizerReplicationPolicy},
		},
		Spec: simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
	}
	r, cl := newPolicyReconciler(t, pair, policy)

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == apiPathReplicationPolicies:
			writeJSON(w, []interface{}{})
		case req.Method == http.MethodPost && req.URL.Path == apiPathReplicationPolicies:
			writeJSON(w, map[string]string{"id": "pol-backend-uuid"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getPolicy(t, cl)
	if got.Status.BackendPolicyID != "pol-backend-uuid" {
		t.Errorf("BackendPolicyID = %q, want pol-backend-uuid", got.Status.BackendPolicyID)
	}
}

// ---------- reuse existing backend policy ----------

func TestPolicy_ReusesExistingBackendPolicy(t *testing.T) {
	pair := readyPairForPolicy("pair1", testClusterName, "tgt-uuid")
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers: []string{utils.FinalizerReplicationPolicy},
		},
		Spec: simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
	}
	r, cl := newPolicyReconciler(t, pair, policy)

	existingPolicies := []interface{}{
		map[string]string{"id": "existing-pol-uuid", "policy_name": "pol"},
	}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			writeJSON(w, existingPolicies)
			return
		}
		// POST must NOT be called.
		t.Error("POST replication/policies should not be called when policy already exists")
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getPolicy(t, cl)
	if got.Status.BackendPolicyID != "existing-pol-uuid" {
		t.Errorf("BackendPolicyID = %q, want existing-pol-uuid", got.Status.BackendPolicyID)
	}
}

// ---------- mark ready ----------

func TestPolicy_MarksReady_WhenPolicyIDPresent(t *testing.T) {
	pair := readyPairForPolicy("pair1", testClusterName, "tgt-uuid")
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers: []string{utils.FinalizerReplicationPolicy},
		},
		Spec:   simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{BackendPolicyID: "bpol"},
	}
	r, cl := newPolicyReconciler(t, pair, policy)
	// BackendPolicyID already set → no API calls needed.
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

func TestPolicy_DeletionBlockedWhileSlotsExist(t *testing.T) {
	now := metav1.Now()
	pair := readyPairForPolicy("pair1", testClusterName, "tgt-uuid")
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPolicy},
			DeletionTimestamp: &now,
		},
		Spec:   simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{BackendPolicyID: "bpol"},
	}
	// A ReplicationSlot still references this policy.
	slot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "slot1", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationSlotSpec{PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "c:p:v"},
	}
	r, cl := newPolicyReconciler(t, pair, policy, slot)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while slots exist, got %+v", res)
	}

	got := getPolicy(t, cl)
	if !contains(got.Finalizers, utils.FinalizerReplicationPolicy) {
		t.Errorf("finalizer was removed prematurely")
	}
}

func TestPolicy_DeletionRemovesBackendPolicyAndFinalizer(t *testing.T) {
	now := metav1.Now()
	pair := readyPairForPolicy("pair1", testClusterName, "tgt-uuid")
	policy := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pol", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPolicy},
			DeletionTimestamp: &now,
		},
		Spec:   simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{BackendPolicyID: "bpol"},
	}
	r, cl := newPolicyReconciler(t, pair, policy)

	deleted := map[string]bool{}
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			deleted[req.URL.Path] = true
		}
		w.WriteHeader(http.StatusOK)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), policyRequest("pol"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !deleted["/api/v2/clusters/"+testClusterUUID+"/replication/policies/bpol"] {
		t.Errorf("backend ReplicationPolicy was not deleted")
	}
	// The backend target is managed by the ReplicationPair controller — must NOT be deleted here.
	if deleted["/api/v2/clusters/"+testClusterUUID+"/replication/targets/tgt-uuid"] {
		t.Errorf("backend ReplicationTarget must not be deleted by the policy controller")
	}

	var gone simplyblockv1alpha1.ReplicationPolicy
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected policy to be GC'd after deletion; err = %v, finalizers = %v", err, gone.Finalizers)
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
