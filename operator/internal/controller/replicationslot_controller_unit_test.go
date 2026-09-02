package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// fakeRecorder satisfies events.EventRecorder for tests.
type fakeRecorder struct{}

func (f *fakeRecorder) Eventf(_ runtime.Object, _ runtime.Object, _, _, _, _ string, _ ...interface{}) {
}

func (f *fakeRecorder) AnnotatedEventf(_ runtime.Object, _ map[string]string, _ runtime.Object, _, _, _, _ string, _ ...interface{}) {
}

// newSlotReconciler creates a ReplicationSlotReconciler backed by a fake client.
// Status for both ReplicationSlot and ReplicationPolicy is pre-seeded via WithObjects.
// The same client is used as apiReader so tests can seed PVs and Pods for consumer-node
// lookup without needing a separate uncached reader.
func newSlotReconciler(t *testing.T, objects ...client.Object) (*ReplicationSlotReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme)
	cl := newTestClient(t, scheme,
		[]client.Object{
			&simplyblockv1alpha1.ReplicationSlot{},
			&simplyblockv1alpha1.ReplicationPolicy{},
		},
		objects...,
	)
	return &ReplicationSlotReconciler{
		Client:    cl,
		Scheme:    scheme,
		Recorder:  &fakeRecorder{},
		apiReader: cl,
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
		return
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

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
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

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
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

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
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
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
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
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
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

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
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

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
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

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
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

// ---------- reconcileReplicating detects cutover_pending and jumps into handler ----------

func TestSlot_ReconcileReplicating_DetectsCutoverPending_TransitionsSlot(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating))

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			// Both reconcileReplicating and reconcileCutoverPending call fetchReplicationStatus.
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			// reconcileCutoverPending fetches connections; returning empty causes an early requeue.
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// reconcileCutoverPending with empty connections returns a 5 s requeue.
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s", res.RequeueAfter)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending) {
		t.Errorf("State = %q, want CutoverPending after backend reported cutover_pending", got.Status.State)
	}
}

// ---------- reconcileCutoverPending: no connections yet → requeue ----------

func TestSlot_CutoverPending_NoConnections_Requeues(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, _ := newSlotReconciler(t, slot, pol)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s when connections not yet available", res.RequeueAfter)
	}
}

// ---------- reconcileCutoverPending: no consumer → signals backend immediately ----------

func TestSlot_CutoverPending_NoConsumer_SignalsImmediately(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))

	var proceedCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			_ = json.NewEncoder(w).Encode([]webapi.LvolConnectResp{{TargetType: "tcp", IP: "1.2.3.4", Port: 4420, Nqn: "nqn.test"}})
		case req.Method == http.MethodPost && strings.HasSuffix(path, "/cutover-proceed"):
			proceedCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	// apiReader (= cl) has no PVs → findConsumerNode returns ""
	r, cl := newSlotReconciler(t, slot, pol)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proceedCalled {
		t.Error("expected POST cutover-proceed to be called when no active consumer exists")
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s", res.RequeueAfter)
	}

	got := getSlot(t, cl)
	if got.Annotations[annotCutoverProceedSignaled] != annotCutoverProceedSignaledValue {
		t.Errorf("annotation %q = %q, want %q", annotCutoverProceedSignaled,
			got.Annotations[annotCutoverProceedSignaled], annotCutoverProceedSignaledValue)
	}
}

// ---------- reconcileCutoverPending: active consumer → creates preconnect Job ----------

func TestSlot_CutoverPending_CreatesJobForConsumer(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))

	// The VolumeID is "cluster-id:pool-id:vol-id"; findConsumerNode looks for
	// a PV whose CSI handle's third segment matches "vol-id".
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: "cluster-id:pool-id:vol-id"},
			},
			ClaimRef: &corev1.ObjectReference{Name: "pvc1", Namespace: "default"},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "worker-1",
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc1"},
				}},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			_ = json.NewEncoder(w).Encode([]webapi.LvolConnectResp{{TargetType: "tcp", IP: "1.2.3.4", Port: 4420, Nqn: "nqn.test"}})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol, pv, pod)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s after creating preconnect Job", res.RequeueAfter)
	}

	expectedJobName := replSlotPreconnectJobName("vol-id")
	var job batchv1.Job
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: expectedJobName}, &job); err != nil {
		t.Fatalf("preconnect Job %q not created: %v", expectedJobName, err)
	}
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "worker-1" {
		t.Errorf("Job NodeSelector hostname = %q, want worker-1",
			job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"])
	}
}

// ---------- reconcileCutoverPending: Job completed → signals proceed, marks annotation ----------

func TestSlot_CutoverPending_JobComplete_Signals(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))
	jobName := replSlotPreconnectJobName("vol-id")
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	var proceedCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			_ = json.NewEncoder(w).Encode([]webapi.LvolConnectResp{{TargetType: "tcp", IP: "1.2.3.4", Port: 4420, Nqn: "nqn.test"}})
		case req.Method == http.MethodPost && strings.HasSuffix(path, "/cutover-proceed"):
			proceedCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol, existingJob)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proceedCalled {
		t.Error("expected POST cutover-proceed after preconnect Job completed successfully")
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s", res.RequeueAfter)
	}
	got := getSlot(t, cl)
	if got.Annotations[annotCutoverProceedSignaled] != annotCutoverProceedSignaledValue {
		t.Errorf("annotation %q not set after job completion", annotCutoverProceedSignaled)
	}
}

// ---------- reconcileCutoverPending: Job failed → signals proceed anyway ----------

func TestSlot_CutoverPending_JobFailed_SignalsAnyway(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))
	jobName := replSlotPreconnectJobName("vol-id")
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}

	var proceedCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			_ = json.NewEncoder(w).Encode([]webapi.LvolConnectResp{{TargetType: "tcp", IP: "1.2.3.4", Port: 4420, Nqn: "nqn.test"}})
		case req.Method == http.MethodPost && strings.HasSuffix(path, "/cutover-proceed"):
			proceedCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol, existingJob)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proceedCalled {
		t.Error("expected POST cutover-proceed even after preconnect Job failed")
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s", res.RequeueAfter)
	}
	got := getSlot(t, cl)
	if got.Annotations[annotCutoverProceedSignaled] != annotCutoverProceedSignaledValue {
		t.Errorf("annotation %q not set after failed job signalling", annotCutoverProceedSignaled)
	}
}

// ---------- reconcileCutoverPending: job still running → waits without signalling ----------

func TestSlot_CutoverPending_JobStillRunning_Waits(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))
	slot.Annotations = map[string]string{annotCutoverProceedSignaled: annotCutoverProceedSignaledValue}
	jobName := replSlotPreconnectJobName("vol-id")
	// Job exists but has no terminal conditions — still running.
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "default"},
		Status:     batchv1.JobStatus{},
	}

	var proceedCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			_ = json.NewEncoder(w).Encode([]webapi.LvolConnectResp{{TargetType: "tcp", IP: "1.2.3.4", Port: 4420, Nqn: "nqn.test"}})
		case req.Method == http.MethodPost && strings.HasSuffix(path, "/cutover-proceed"):
			proceedCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, _ := newSlotReconciler(t, slot, pol, existingJob)

	res, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proceedCalled {
		t.Error("POST cutover-proceed must not be called while the preconnect job is still running")
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("RequeueAfter = %v, want 5s while waiting for job to finish", res.RequeueAfter)
	}
}

// ---------- reconcileCutoverPending: previously-failed proceed retried when job completes again ----------

func TestSlot_CutoverPending_ProceedRetried_WhenJobTerminates(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))
	// Annotation already set (from a previous cycle where callCutoverProceed failed).
	slot.Annotations = map[string]string{annotCutoverProceedSignaled: annotCutoverProceedSignaledValue}
	jobName := replSlotPreconnectJobName("vol-id")
	// A completed job — simulates a new job created after the first proceed failed.
	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	var proceedCalled bool
	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{State: backendStateCutoverPending})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			_ = json.NewEncoder(w).Encode([]webapi.LvolConnectResp{{TargetType: "tcp", IP: "1.2.3.4", Port: 4420, Nqn: "nqn.test"}})
		case req.Method == http.MethodPost && strings.HasSuffix(path, "/cutover-proceed"):
			proceedCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, _ := newSlotReconciler(t, slot, pol, completedJob)

	_, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proceedCalled {
		t.Error("POST cutover-proceed must be retried when a completed job is seen, even if annotation was already set (prior proceed may have failed)")
	}
}

// ---------- reconcileCutoverPending: backend already cutover_done → advances slot state ----------

func TestSlot_CutoverPending_BackendAlreadyCutoverDone_AppliesState(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending))

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{
				State: backendStateCutoverDone, TargetNQN: "nqn.target",
			})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			// reconcilePreconnect fetches connections; return empty to short-circuit it.
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	_, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone) {
		t.Errorf("State = %q, want CutoverDone when backend reports cutover_done", got.Status.State)
	}
	if got.Status.Direction != string(simplyblockv1alpha1.ReplicationSlotDirectionTarget) {
		t.Errorf("Direction = %q, want Target after cutover_done", got.Status.Direction)
	}
	if got.Status.TargetNQN != "nqn.target" {
		t.Errorf("TargetNQN = %q, want nqn.target", got.Status.TargetNQN)
	}
}

func TestSlot_ReconcileReplicating_DetectsCutoverDone_SetsDirectionTarget(t *testing.T) {
	pol := readyReplicationPolicy()
	slot := newTestSlot(string(simplyblockv1alpha1.ReplicationSlotStateReplicating))

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/replication"):
			_ = json.NewEncoder(w).Encode(replVolumeReplicationStatus{
				State: backendStateCutoverDone, TargetNQN: "nqn.target",
			})
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/connect"):
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	r, cl := newSlotReconciler(t, slot, pol)

	_, err := r.Reconcile(context.Background(), slotRequest("slot1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getSlot(t, cl)
	if got.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverDone) {
		t.Errorf("State = %q, want CutoverDone", got.Status.State)
	}
	if got.Status.Direction != string(simplyblockv1alpha1.ReplicationSlotDirectionTarget) {
		t.Errorf("Direction = %q, want Target after cutover_done", got.Status.Direction)
	}
}
