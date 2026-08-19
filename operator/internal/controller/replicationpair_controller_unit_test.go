package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// newPairReconciler creates a ReplicationPairReconciler backed by a fake client.
func newPairReconciler(t *testing.T, objects ...client.Object) (*ReplicationPairReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := newTestClient(t, scheme,
		[]client.Object{&simplyblockv1alpha1.ReplicationPair{}, &simplyblockv1alpha1.ReplicationPolicy{}},
		objects...,
	)
	return &ReplicationPairReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
	}, cl
}

func pairRequest(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func getPair(t *testing.T, cl client.Client, ns, name string) *simplyblockv1alpha1.ReplicationPair {
	t.Helper()
	p := &simplyblockv1alpha1.ReplicationPair{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, p); err != nil {
		t.Fatalf("get ReplicationPair: %v", err)
	}
	return p
}

func readyPolicyWithIDs(name, ns string) *simplyblockv1alpha1.ReplicationPolicy {
	return &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{Target: "cluster-a"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{
			Ready:           true,
			BackendPolicyID: "bpol-uuid",
			BackendTargetID: "tgt-uuid",
		},
	}
}

func newPair(name, ns, policyRef, volumeID string) *simplyblockv1alpha1.ReplicationPair {
	return &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  ns,
			Finalizers: []string{utils.FinalizerReplicationPair},
		},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: policyRef,
			PVCRef:    "pvc1",
			VolumeID:  volumeID,
		},
	}
}

// replicationStatusJSON returns a JSON-encoded backend replication status.
func replicationStatusJSON(state, direction, sourceLvol, targetLvol string, isSource bool) string {
	rs := replVolumeReplicationStatus{
		State:        state,
		Direction:    direction,
		SourceLvolID: sourceLvol,
		TargetLvolID: targetLvol,
		IsSource:     isSource,
	}
	b, _ := json.Marshal(rs)
	return string(b)
}

// ---------- ignore not-found ----------

func TestPair_IgnoreNotFound(t *testing.T) {
	r, _ := newPairReconciler(t)
	res, err := r.Reconcile(context.Background(), pairRequest("default", "missing"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}
}

// ---------- finalizer ----------

func TestPair_AddsFinalizer(t *testing.T) {
	pol := readyPolicyWithIDs("pol", "default")
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: "pair1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	r, cl := newPairReconciler(t, pol, pair)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	// The reconciler will add the finalizer then return (before hitting the API).
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")
	_, _ = r.Reconcile(context.Background(), pairRequest("default", "pair1"))

	got := getPair(t, cl, "default", "pair1")
	if !containsString(got.Finalizers, utils.FinalizerReplicationPair) {
		t.Errorf("finalizer not added; finalizers = %v", got.Finalizers)
	}
}

// ---------- policy not found → error state ----------

func TestPair_PolicyNotFound_SetsError(t *testing.T) {
	pair := newPair("pair1", "default", "pol", "c:p:v")
	r, cl := newPairReconciler(t, pair)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replPairRequeueError {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replPairRequeueError)
	}
	got := getPair(t, cl, "default", "pair1")
	if got.Status.State != string(simplyblockv1alpha1.ReplicationPairStateError) {
		t.Errorf("state = %q, want error", got.Status.State)
	}
}

// ---------- policy not ready → wait ----------

func TestPair_PolicyNotReady_Waits(t *testing.T) {
	pol := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{Target: "cluster-a"},
		Status:     simplyblockv1alpha1.ReplicationPolicyStatus{Ready: false},
	}
	pair := newPair("pair1", "default", "pol", "c:p:v")
	r, cl := newPairReconciler(t, pol, pair)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while policy not ready")
	}
}

// ---------- invalid volume handle → error state ----------

func TestPair_InvalidVolumeHandle_SetsError(t *testing.T) {
	pol := readyPolicyWithIDs("pol", "default")
	pair := newPair("pair1", "default", "pol", "bad-handle")
	r, cl := newPairReconciler(t, pol, pair)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replPairRequeueError {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replPairRequeueError)
	}
	got := getPair(t, cl, "default", "pair1")
	if got.Status.State != string(simplyblockv1alpha1.ReplicationPairStateError) {
		t.Errorf("state = %q, want error", got.Status.State)
	}
}

// ---------- attach success → state = attaching ----------

func TestPair_Attach_Success(t *testing.T) {
	pol := readyPolicyWithIDs("pol", "default")
	pair := newPair("pair1", "default", "pol", "cluster-u:pool-u:vol-u")
	r, cl := newPairReconciler(t, pol, pair)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	res, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replPairRequeueAttaching {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replPairRequeueAttaching)
	}
	got := getPair(t, cl, "default", "pair1")
	if got.Status.State != string(simplyblockv1alpha1.ReplicationPairStateAttaching) {
		t.Errorf("state = %q, want attaching", got.Status.State)
	}
}

// ---------- attach backend error → error state ----------

func TestPair_Attach_BackendError(t *testing.T) {
	pol := readyPolicyWithIDs("pol", "default")
	pair := newPair("pair1", "default", "pol", "cluster-u:pool-u:vol-u")
	r, cl := newPairReconciler(t, pol, pair)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getPair(t, cl, "default", "pair1")
	if got.Status.State != string(simplyblockv1alpha1.ReplicationPairStateError) {
		t.Errorf("state = %q, want error", got.Status.State)
	}
}

// ---------- poll attach: not yet replicating → stays attaching ----------

func TestPair_PollAttach_WaitsIfNotReplicating(t *testing.T) {
	pol := readyPolicyWithIDs("pol", "default")
	pair := newPair("pair1", "default", "pol", "cluster-u:pool-u:vol-u")
	pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateAttaching)
	r, cl := newPairReconciler(t, pol, pair)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), pair); err != nil {
		t.Fatalf("pre-set pair status: %v", err)
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		// Backend still in "attaching" state.
		writeJSON(w, http.StatusOK, replVolumeReplicationStatus{State: "attaching"})
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	res, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replPairRequeueAttaching {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replPairRequeueAttaching)
	}
	got := getPair(t, cl, "default", "pair1")
	if got.Status.State != string(simplyblockv1alpha1.ReplicationPairStateAttaching) {
		t.Errorf("state = %q, want still attaching", got.Status.State)
	}
}

// ---------- poll attach: backend reaches replicating → advances ----------

func TestPair_PollAttach_AdvancesToReplicating(t *testing.T) {
	pol := readyPolicyWithIDs("pol", "default")
	pair := newPair("pair1", "default", "pol", "cluster-u:pool-u:vol-u")
	pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateAttaching)
	r, cl := newPairReconciler(t, pol, pair)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), pair); err != nil {
		t.Fatalf("pre-set pair status: %v", err)
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, replVolumeReplicationStatus{
			State:        utils.ReplicationBackendStateReplicating,
			SourceLvolID: "src-lvol",
			TargetLvolID: "tgt-lvol",
			IsSource:     true,
		})
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	res, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replPairRequeueReplicating {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replPairRequeueReplicating)
	}
	got := getPair(t, cl, "default", "pair1")
	if got.Status.State != string(simplyblockv1alpha1.ReplicationPairStateReplicating) {
		t.Errorf("state = %q, want replicating", got.Status.State)
	}
	if got.Status.SourceLvolID != "src-lvol" {
		t.Errorf("SourceLvolID = %q, want src-lvol", got.Status.SourceLvolID)
	}
	if got.Status.Direction != "source" {
		t.Errorf("Direction = %q, want source", got.Status.Direction)
	}
}

// ---------- detach success → finalizer removed ----------

func TestPair_Detach_Success(t *testing.T) {
	now := metav1.Now()
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pair1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPair},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "cluster-u:pool-u:vol-u",
		},
	}
	r, cl := newPairReconciler(t, pair)

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After removing the last finalizer from an object with DeletionTimestamp,
	// the fake client GCs the object. Verify it's gone.
	var gone simplyblockv1alpha1.ReplicationPair
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pair1"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected pair to be GC'd after detach; err = %v, finalizers = %v", err, gone.Finalizers)
	}
}

// ---------- detach 404 → treated as success ----------

func TestPair_Detach_NotFound_Succeeds(t *testing.T) {
	now := metav1.Now()
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pair1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPair},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "cluster-u:pool-u:vol-u",
		},
	}
	r, cl := newPairReconciler(t, pair)

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gone simplyblockv1alpha1.ReplicationPair
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pair1"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected pair to be GC'd after 404 detach; err = %v, finalizers = %v", err, gone.Finalizers)
	}
}

// ---------- detach 409 → wait (cutover in flight) ----------

func TestPair_Detach_Conflict_Waits(t *testing.T) {
	now := metav1.Now()
	pair := &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pair1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationPair},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "cluster-u:pool-u:vol-u",
		},
	}
	r, cl := newPairReconciler(t, pair)

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	res, err := r.Reconcile(context.Background(), pairRequest("default", "pair1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue after 409 conflict, got %+v", res)
	}

	// Finalizer must still be present.
	got := getPair(t, cl, "default", "pair1")
	if !containsString(got.Finalizers, utils.FinalizerReplicationPair) {
		t.Errorf("finalizer was removed on 409 conflict")
	}
}

// ---------- splitVolumeHandle ----------

func TestSplitVolumeHandle(t *testing.T) {
	cases := []struct {
		input  string
		wantC  string
		wantP  string
		wantV  string
		wantOK bool
	}{
		{"cluster-uuid:pool-uuid:vol-uuid", "cluster-uuid", "pool-uuid", "vol-uuid", true},
		{"a:b:c", "a", "b", "c", true},
		{"only-two:parts", "", "", "", false},
		{"no-colons", "", "", "", false},
		{"::empty", "", "", "", false},
		{":b:c", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			c, p, v, ok := splitVolumeHandle(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok {
				if c != tc.wantC || p != tc.wantP || v != tc.wantV {
					t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", c, p, v, tc.wantC, tc.wantP, tc.wantV)
				}
			}
		})
	}
}

// ---------- parseLastSnapshotAt ----------

func TestParseLastSnapshotAt(t *testing.T) {
	ts := "2025-01-15T10:00:00Z"
	cases := []struct {
		input   interface{}
		wantNil bool
	}{
		{ts, false},
		{nil, true},
		{"", true},
		{"not-a-date", true},
		{42, true},
	}
	for _, tc := range cases {
		got := parseLastSnapshotAt(tc.input)
		if tc.wantNil && got != nil {
			t.Errorf("parseLastSnapshotAt(%v) = %v, want nil", tc.input, got)
		}
		if !tc.wantNil && got == nil {
			t.Errorf("parseLastSnapshotAt(%v) = nil, want non-nil", tc.input)
		}
	}

	// Verify parsed value.
	got := parseLastSnapshotAt(ts)
	want := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	if got == nil || !got.Equal(want) {
		t.Errorf("parsed time = %v, want %v", got, want)
	}
}
