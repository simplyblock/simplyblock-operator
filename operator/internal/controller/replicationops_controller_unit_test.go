package controller

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// newOpsReplReconciler creates a ReplicationOpsReconciler backed by a fake client.
// A StorageCluster (testClusterName → testClusterUUID) is pre-populated so that
// reconcileFailover can resolve the cluster UUID via utils.ResolveClusterUUID.
// Field indexes match what SetupWithManager registers:
//   - ReplicationSlot.spec.policyRef (for collectAffectedSlots)
//   - ReplicationOps.spec.ref (for policyToOpsRequests)
func newOpsReplReconciler(t *testing.T, objects ...client.Object) (*ReplicationOpsReconciler, client.Client) {
	t.Helper()
	localCluster := testCluster("default", testClusterName, testClusterUUID)
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	allObjects := append([]client.Object{localCluster}, objects...)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.ReplicationOps{},
			&simplyblockv1alpha1.ReplicationPolicy{},
			&simplyblockv1alpha1.ReplicationSlot{},
			&simplyblockv1alpha1.ReplicationPair{},
		).
		WithObjects(allObjects...).
		WithIndex(&simplyblockv1alpha1.ReplicationSlot{}, "spec.policyRef", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationSlot).Spec.PolicyRef}
		}).
		WithIndex(&simplyblockv1alpha1.ReplicationOps{}, "spec.ref", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationOps).Spec.Ref}
		}).
		WithIndex(&simplyblockv1alpha1.ReplicationPolicy{}, "spec.pairRef", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationPolicy).Spec.PairRef}
		}).
		Build()
	return &ReplicationOpsReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: &fakeRecorder{},
	}, cl
}

// readyPairForOps returns a ReplicationPair named "pair1" that is ready.
// All policies created by readyPolicyForOps reference this pair via PairRef: "pair1".
func readyPairForOps() *simplyblockv1alpha1.ReplicationPair {
	return &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: "pair1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			SourceCluster: testClusterName,
			TargetCluster: "cluster-b",
		},
		Status: simplyblockv1alpha1.ReplicationPairStatus{
			Ready:           true,
			BackendTargetID: "tgt-uuid",
		},
	}
}

func opsRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func getOps(t *testing.T, cl client.Client) *simplyblockv1alpha1.ReplicationOps {
	t.Helper()
	o := &simplyblockv1alpha1.ReplicationOps{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "ops1"}, o); err != nil {
		t.Fatalf("get ReplicationOps: %v", err)
	}
	return o
}

// readyPolicyForOps returns a ready ReplicationPolicy with BackendPolicyID set.
func readyPolicyForOps(name string) *simplyblockv1alpha1.ReplicationPolicy {
	return &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{PairRef: "pair1"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{
			Ready:           true,
			BackendPolicyID: "bpol-uuid",
		},
	}
}

// slotForPolicy creates a ReplicationSlot that belongs to policy "pol" with a valid volume handle.
func slotForPolicy() *simplyblockv1alpha1.ReplicationSlot {
	return &simplyblockv1alpha1.ReplicationSlot{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-pvc1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationSlotSpec{
			PolicyRef: "pol",
			PVCRef:    "pvc1",
			VolumeID:  "cluster-u:pool-u:vol-u",
		},
		Status: simplyblockv1alpha1.ReplicationSlotStatus{
			State:        string(simplyblockv1alpha1.ReplicationSlotStateReplicating),
			TargetLvolID: "tgt-lvol",
		},
	}
}

// ---------- ignore not-found ----------

func TestOps_IgnoreNotFound(t *testing.T) {
	r, _ := newOpsReplReconciler(t)
	res, err := r.Reconcile(context.Background(), opsRequest("missing"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}
}

// ---------- finalizer ----------

func TestOps_AddsFinalizer(t *testing.T) {
	pol := readyPolicyForOps("pol")
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops1", Namespace: "default"},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")
	_, _ = r.Reconcile(context.Background(), opsRequest("ops1"))

	got := getOps(t, cl)
	if !contains(got.Finalizers, finalizerReplicationOps) {
		t.Errorf("finalizer not added; finalizers = %v", got.Finalizers)
	}
}

// ---------- terminal phases — no-op ----------

func TestOps_TerminalSucceeded_NoOp(t *testing.T) {
	pol := readyPolicyForOps("pol")
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec:   simplyblockv1alpha1.ReplicationOpsSpec{Action: "failover", Scope: "policy", Ref: "pol"},
		Status: simplyblockv1alpha1.ReplicationOpsStatus{Phase: string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded)},
	}
	r, _ := newOpsReplReconciler(t, pol, ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal phase, got %+v", res)
	}
}

func TestOps_TerminalFailed_NoOp(t *testing.T) {
	pol := readyPolicyForOps("pol")
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec:   simplyblockv1alpha1.ReplicationOpsSpec{Action: "failover", Scope: "policy", Ref: "pol"},
		Status: simplyblockv1alpha1.ReplicationOpsStatus{Phase: string(simplyblockv1alpha1.ReplicationOpsPhaseFailed)},
	}
	r, _ := newOpsReplReconciler(t, pol, ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal phase, got %+v", res)
	}
}

// ---------- policy not found → fails ops ----------

func TestOps_PolicyNotFound_Fails(t *testing.T) {
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopePolicy, Ref: "nonexistent",
		},
	}
	r, cl := newOpsReplReconciler(t, ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ---------- mutual exclusion: another ops holds the lock ----------

func TestOps_MutualExclusion_WaitsIfLocked(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pol.Status.ActiveOpsRef = "other-ops" // set before WithObjects so it's seeded
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while policy lock is held by another ops")
	}
	got := getOps(t, cl)
	if got.Status.Phase == string(simplyblockv1alpha1.ReplicationOpsPhaseRunning) {
		t.Errorf("ops phase advanced to Running while lock was held by another")
	}
}

// ---------- unknown action → fails ----------

func TestOps_UnknownAction_Fails(t *testing.T) {
	pol := readyPolicyForOps("pol")
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "rollback", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ---------- failover scope=policy success ----------

func TestOps_Failover_ScopePolicy_Success(t *testing.T) {
	pol := readyPolicyForOps("pol")
	slot := slotForPolicy()
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol, slot, ops)

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if len(got.Status.Results) != 1 {
		t.Errorf("results = %d, want 1", len(got.Status.Results))
	}
}

// ---------- failover scope=target success ----------

func TestOps_Failover_ScopeTarget_Success(t *testing.T) {
	pol := readyPolicyForOps("pol")
	slot := slotForPolicy()
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopeTarget, Ref: "pair1",
		},
	}
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol, slot, ops)

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
}

// ---------- failover scope=volume success ----------

func TestOps_Failover_ScopeVolume_Success(t *testing.T) {
	// For scope=volume, ops.Spec.Ref is the slot name.
	// resolveAffectedPolicyName also returns ops.Spec.Ref, so the reconciler
	// tries to find a policy with that name. Create one as a workaround.
	pol := readyPolicyForOps("pol")
	slot := slotForPolicy() // named "pol-pvc1"
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopeVolume, Ref: "pol-pvc1",
		},
	}
	polForVolume := readyPolicyForOps("pol-pvc1")
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol, slot, ops, polForVolume)

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
}

// ---------- failback success ----------

func TestOps_Failback_Success(t *testing.T) {
	pol := readyPolicyForOps("pol")
	slot := slotForPolicy()
	slot.Status.State = string(simplyblockv1alpha1.ReplicationSlotStateFailedOver)
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failback", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, slot, ops)

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if len(got.Status.Results) != 1 || got.Status.Results[0].Status != string(simplyblockv1alpha1.ReplicationOpsResultSucceeded) {
		t.Errorf("unexpected results: %+v", got.Status.Results)
	}

	// Slot direction should be updated back to source.
	gotSlot := &simplyblockv1alpha1.ReplicationSlot{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-pvc1"}, gotSlot); err != nil {
		t.Fatalf("get slot: %v", err)
	}
	if gotSlot.Status.Direction != string(simplyblockv1alpha1.ReplicationSlotDirectionSource) {
		t.Errorf("slot direction = %q, want source after failback", gotSlot.Status.Direction)
	}
}

// ---------- failback partial failure ----------

func TestOps_Failback_PartialFailure(t *testing.T) {
	pol := readyPolicyForOps("pol")
	slot := slotForPolicy()
	slot.Spec.VolumeID = "bad-handle" // invalid handle → fails failback
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failback", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, slot, ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed when a slot's VolumeID is invalid", got.Status.Phase)
	}
}

// ---------- succeedOps releases lock ----------

func TestOps_SucceedOps_ReleasesLock(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pol.Status.ActiveOpsRef = "ops1"
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec:   simplyblockv1alpha1.ReplicationOpsSpec{Action: "failover", Scope: "policy", Ref: "pol"},
		Status: simplyblockv1alpha1.ReplicationOpsStatus{Phase: string(simplyblockv1alpha1.ReplicationOpsPhaseRunning)},
	}
	r, cl := newOpsReplReconciler(t, pol, ops)

	_, err := r.succeedOps(context.Background(), ops, "done", nil)
	if err != nil {
		t.Fatalf("succeedOps: %v", err)
	}

	p := &simplyblockv1alpha1.ReplicationPolicy{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if p.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want empty after succeedOps", p.Status.ActiveOpsRef)
	}
}

// ---------- migration action ----------

func TestOps_Migration_ScopePolicy_Success(t *testing.T) {
	pol := readyPolicyForOps("pol")
	slot := slotForPolicy()
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol, slot, ops)

	srv := newAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted) // 202 = commit accepted
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if len(got.Status.Results) != 1 || got.Status.Results[0].Status != string(simplyblockv1alpha1.ReplicationOpsResultSucceeded) {
		t.Errorf("results = %+v, want 1 Succeeded result", got.Status.Results)
	}

	// reconcileMigration immediately patches the slot to cutover_pending once commit is accepted.
	gotSlot := &simplyblockv1alpha1.ReplicationSlot{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-pvc1"}, gotSlot); err != nil {
		t.Fatalf("get slot: %v", err)
	}
	if gotSlot.Status.State != string(simplyblockv1alpha1.ReplicationSlotStateCutoverPending) {
		t.Errorf("slot state = %q, want CutoverPending after commit accepted", gotSlot.Status.State)
	}
}

func TestOps_Migration_PairNotFound_Fails(t *testing.T) {
	// readyPolicyForOps references "pair1" but no ReplicationPair CR is provided.
	pol := readyPolicyForOps("pol")
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, ops) // no pair
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed when ReplicationPair not found", got.Status.Phase)
	}
}

func TestOps_Migration_BackendError_Fails(t *testing.T) {
	pol := readyPolicyForOps("pol")
	slot := slotForPolicy()
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol, slot, ops)

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed when backend returns 500", got.Status.Phase)
	}
}

func TestOps_Migration_InvalidVolumeID_Fails(t *testing.T) {
	pol := readyPolicyForOps("pol")
	slot := slotForPolicy()
	slot.Spec.VolumeID = "bad-handle" // invalid: only two colon-separated parts
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "migration", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, readyPairForOps(), pol, slot, ops)
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed for slot with invalid VolumeID", got.Status.Phase)
	}
}

// ---------- failOps releases lock ----------

func TestOps_FailOps_ReleasesLock(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pol.Status.ActiveOpsRef = "ops1"
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec:   simplyblockv1alpha1.ReplicationOpsSpec{Action: "failover", Scope: "policy", Ref: "pol"},
		Status: simplyblockv1alpha1.ReplicationOpsStatus{Phase: string(simplyblockv1alpha1.ReplicationOpsPhaseRunning)},
	}
	r, cl := newOpsReplReconciler(t, pol, ops)

	_, err := r.failOps(context.Background(), ops, "something went wrong")
	if err != nil {
		t.Fatalf("failOps: %v", err)
	}

	p := &simplyblockv1alpha1.ReplicationPolicy{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if p.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want empty after failOps", p.Status.ActiveOpsRef)
	}

	got := getOps(t, cl)
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("ops phase = %q, want Failed", got.Status.Phase)
	}
}
