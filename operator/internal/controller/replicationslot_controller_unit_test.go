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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/ctrltest"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// fakeRecorder satisfies events.EventRecorder for tests.
type fakeRecorder struct{}

func (f *fakeRecorder) Eventf(_ runtime.Object, _ runtime.Object, _, _, _, _ string, _ ...interface{}) {
}

func (f *fakeRecorder) AnnotatedEventf(_ runtime.Object, _ map[string]string, _ runtime.Object, _, _, _, _ string, _ ...interface{}) {
}

// newSlotReconciler creates a ReplicationSlotReconciler backed by a fake client.
// Status for both ReplicationSlot and ReplicationPolicy is pre-seeded via WithObjects.
func newSlotReconciler(t *testing.T, objects ...client.Object) (*ReplicationSlotReconciler, client.Client) {
	t.Helper()
	scheme := ctrltest.NewScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := ctrltest.NewClient(t, scheme,
		[]client.Object{
			&simplyblockv1alpha1.ReplicationSlot{},
			&simplyblockv1alpha1.ReplicationPolicy{},
		},
		objects...,
	)
	return &ReplicationSlotReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: &fakeRecorder{},
	}, cl
}

func slotRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func getSlot(t *testing.T, cl client.Client) *simplyblockv1alpha1.ReplicationSlot {
	t.Helper()
	s := &simplyblockv1alpha1.ReplicationSlot{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "slot1"}, s); err != nil {
		t.Fatalf("get ReplicationSlot: %v", err)
	}
	return s
}

// readyReplicationPolicy returns a ReplicationPolicy named "pol" with BackendPolicyID set and Ready=true.
// Status is pre-seeded so the fake client's WithObjects preserves it on Get.
func readyReplicationPolicy() *simplyblockv1alpha1.ReplicationPolicy {
	return &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{
			Ready:           true,
			BackendPolicyID: "pol-backend-id",
		},
	}
}

// newTestSlot creates a ReplicationSlot in the given state.
// Passing "" as state creates a brand-new slot (triggers reconcileAttach).
func newTestSlot(state string) *simplyblockv1alpha1.ReplicationSlot {
	s := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "slot1",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerReplicationSlot},
		},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol",
			PVCRef:    "pvc1",
			VolumeID:  "cluster-id:pool-id:vol-id",
		},
	}
	if state != "" {
		s.Status.State = state
	}
	return s
}

// ---------- splitVolumeHandle ----------

func TestSplitVolumeHandle(t *testing.T) {
	cases := []struct {
		input       string
		wantCluster string
		wantPool    string
		wantVol     string
		wantOK      bool
	}{
		{"cluster:pool:volume", "cluster", "pool", "volume", true},
		{"a:b:c", "a", "b", "c", true},
		{"only-two:parts", "", "", "", false},
		{"", "", "", "", false},
		{":pool:vol", "", "", "", false},
		{"cluster::vol", "", "", "", false},
		{"cluster:pool:", "", "", "", false},
	}
	for _, tc := range cases {
		c, p, v, ok := splitVolumeHandle(tc.input)
		if ok != tc.wantOK {
			t.Errorf("splitVolumeHandle(%q) ok=%v, want %v", tc.input, ok, tc.wantOK)
			continue
		}
		if ok && (c != tc.wantCluster || p != tc.wantPool || v != tc.wantVol) {
			t.Errorf("splitVolumeHandle(%q) = (%q,%q,%q), want (%q,%q,%q)",
				tc.input, c, p, v, tc.wantCluster, tc.wantPool, tc.wantVol)
		}
	}
}

// ---------- parseLastSnapshotAt ----------

func TestParseLastSnapshotAt(t *testing.T) {
	ts := parseLastSnapshotAt("2024-01-15T10:30:00Z")
	if ts == nil {
		t.Fatal("expected non-nil time for valid RFC3339 string")
	}
	if ts.Year() != 2024 {
		t.Errorf("year = %d, want 2024", ts.Year())
	}

	if parseLastSnapshotAt(nil) != nil {
		t.Error("expected nil for nil input")
	}
	if parseLastSnapshotAt("") != nil {
		t.Error("expected nil for empty string")
	}
	if parseLastSnapshotAt("not-a-date") != nil {
		t.Error("expected nil for invalid date")
	}
	if parseLastSnapshotAt(42) != nil {
		t.Error("expected nil for non-string input")
	}
}

// ---------- ignore not-found ----------

func TestSlot_IgnoreNotFound(t *testing.T) {
	r, _ := newSlotReconciler(t)
	res, err := r.Reconcile(context.Background(), slotRequest("missing"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

// ---------- adds finalizer on first reconcile ----------

func TestSlot_AddsFinalizer(t *testing.T) {
	slot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "slot1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "c:p:v",
		},
	}
	r, cl := newSlotReconciler(t, slot)
	_, _ = r.Reconcile(context.Background(), slotRequest("slot1"))

	got := getSlot(t, cl)
	if !contains(got.Finalizers, utils.FinalizerReplicationSlot) {
		t.Errorf("finalizer not added; finalizers = %v", got.Finalizers)
	}
}

// ---------- reconcileAttach: sets state to replicating on success ----------

func TestSlot_ReconcileAttach_Success(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot("") // state="" triggers reconcileAttach

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replSlotRequeueReplicating {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replSlotRequeueReplicating)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q, want Replicating", got.Status.State)
	}
	if got.Status.Direction != string(simplyblockv1alpha1.ReplicationSlotDirectionSource) {
		t.Errorf("Direction = %q, want Source", got.Status.Direction)
	}
}

// ---------- reconcileAttach: failure sets state to error ----------

func TestSlot_ReconcileAttach_Failure_SetsError(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot("")

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replSlotRequeueError {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replSlotRequeueError)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateError) {
		t.Errorf("State = %q, want Error", got.Status.State)
	}
}

// ---------- policy not ready → waits ----------

func TestSlot_PolicyNotReady_Waits(t *testing.T) {
	pol := &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status:     simplyblockv1alpha1.ReplicationPolicyStatus{Ready: false},
	}
	slot := newTestSlot("")

	r, _ := newSlotReconciler(t, slot, pol)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")
	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue when policy not ready")
	}
}

// ---------- invalid volume handle → error state ----------

func TestSlot_InvalidVolumeHandle_SetsError(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "slot1",
			Namespace:  "default",
			Finalizers: []string{utils.FinalizerReplicationSlot},
		},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "bad-handle",
		},
	}

	r, cl := newSlotReconciler(t, slot, pol)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")
	_, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateError) {
		t.Errorf("State = %q, want Error for bad volume handle", got.Status.State)
	}
}

// ---------- reconcilePollAttach: attaching → replicating ----------

func TestSlot_ReconcilePollAttach_TransitionsToReplicating(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateAttaching))

	r, cl := newSlotReconciler(t, slot, pol)

	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")
	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replSlotRequeueReplicating {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replSlotRequeueReplicating)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q, want Replicating", got.Status.State)
	}
	if got.Status.Direction != string(simplyblockv1alpha1.ReplicationSlotDirectionSource) {
		t.Errorf("Direction = %q, want Source", got.Status.Direction)
	}
}

// ---------- reconcileReplicating: backend 404 (normal) → keep replicating ----------

func TestSlot_ReconcileReplicating_NotFound_KeepsPolling(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating))

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replSlotRequeueReplicating {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replSlotRequeueReplicating)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State changed unexpectedly to %q", got.Status.State)
	}
}

// ---------- reconcileReplicating: backend returns failed_over → updates state ----------

func TestSlot_ReconcileReplicating_FailedOver_UpdatesState(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating))

	backendStatus := replVolumeReplicationStatus{
		State:     "failed_over",
		Direction: "target",
		TargetNQN: "nqn.test",
		IsSource:  false,
	}
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(backendStatus)
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	_, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateFailedOver) {
		t.Errorf("State = %q, want FailedOver", got.Status.State)
	}
	if got.Status.Direction != string(simplyblockv1alpha1.ReplicationSlotDirectionTarget) {
		t.Errorf("Direction = %q, want Target", got.Status.Direction)
	}
	if got.Status.TargetNQN != "nqn.test" {
		t.Errorf("TargetNQN = %q, want nqn.test", got.Status.TargetNQN)
	}
}

// ---------- reconcileReplicating: lastSnapshotAt updated ----------

func TestSlot_ReconcileReplicating_UpdatesLastReplicatedAt(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating))

	snapshotTime := "2024-06-01T12:00:00Z"
	backendStatus := map[string]interface{}{
		"state":            utils.ReplicationBackendStateReplicating,
		"last_snapshot_at": snapshotTime,
	}
	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(backendStatus)
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	_, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getSlot(t, cl)
	if got.Status.LastReplicatedAt == nil {
		t.Errorf("expected LastReplicatedAt to be set")
	} else {
		want, _ := time.Parse(time.RFC3339, snapshotTime)
		if !got.Status.LastReplicatedAt.Time.Equal(want) {
			t.Errorf("LastReplicatedAt = %v, want %v", got.Status.LastReplicatedAt.Time, want)
		}
	}
}

// ---------- reconcileDetach: success removes finalizer ----------

func TestSlot_ReconcileDetach_RemovesFinalizer(t *testing.T) {
	now := metav1.Now()
	slot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "slot1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationSlot},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "cluster-id:pool-id:vol-id",
		},
	}
	pol := readyReplicationPolicy()

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	_, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After removing the last finalizer from an object with DeletionTimestamp, the fake
	// client GC's the object entirely (correct Kubernetes behavior).
	var gone simplyblockv1alpha1.ReplicationSlot
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "slot1"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected slot to be GC'd after finalizer removal; got finalizers = %v", gone.Finalizers)
	}
}

// ---------- reconcileDetach: PUT fails → requeues, keeps finalizer ----------

func TestSlot_ReconcileDetach_Failure_KeepsFinalizer(t *testing.T) {
	now := metav1.Now()
	slot := &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "slot1", Namespace: "default",
			Finalizers:        []string{utils.FinalizerReplicationSlot},
			DeletionTimestamp: &now,
		},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol", PVCRef: "pvc1", VolumeID: "cluster-id:pool-id:vol-id",
		},
	}
	pol := readyReplicationPolicy()

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != replSlotRequeueError {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replSlotRequeueError)
	}

	got := getSlot(t, cl)
	if !contains(got.Finalizers, utils.FinalizerReplicationSlot) {
		t.Errorf("finalizer removed despite detach failure")
	}
}

// ---------- error state retries attach ----------

func TestSlot_ErrorState_RetriesAttach(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateError))

	srv := ctrltest.NewAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateReplicating) {
		t.Errorf("State = %q after retry; want Replicating", got.Status.State)
	}
	if res.RequeueAfter != replSlotRequeueReplicating {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, replSlotRequeueReplicating)
	}
}
