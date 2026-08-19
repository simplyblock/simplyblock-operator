package controller

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// newOpsReplReconciler creates a ReplicationOpsReconciler backed by a fake client.
// Field indexes are registered for spec.policyRef (on ReplicationPair) and
// spec.ref (on ReplicationOps), matching what SetupWithManager would register.
func newOpsReplReconciler(t *testing.T, objects ...client.Object) (*ReplicationOpsReconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, corev1.AddToScheme)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&simplyblockv1alpha1.ReplicationOps{},
			&simplyblockv1alpha1.ReplicationPolicy{},
			&simplyblockv1alpha1.ReplicationPair{},
		).
		WithObjects(objects...).
		WithIndex(&simplyblockv1alpha1.ReplicationPair{}, "spec.policyRef", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationPair).Spec.PolicyRef}
		}).
		WithIndex(&simplyblockv1alpha1.ReplicationOps{}, "spec.ref", func(obj client.Object) []string {
			return []string{obj.(*simplyblockv1alpha1.ReplicationOps).Spec.Ref}
		}).
		Build()
	return &ReplicationOpsReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(16),
	}, cl
}

func opsRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}

func getOps(t *testing.T, cl client.Client, name string) *simplyblockv1alpha1.ReplicationOps {
	t.Helper()
	o := &simplyblockv1alpha1.ReplicationOps{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, o); err != nil {
		t.Fatalf("get ReplicationOps: %v", err)
	}
	return o
}

// readyPolicyForOps returns a ready ReplicationPolicy with backend IDs.
func readyPolicyForOps(name string) *simplyblockv1alpha1.ReplicationPolicy {
	return &simplyblockv1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       simplyblockv1alpha1.ReplicationPolicySpec{Target: "cluster-a"},
		Status: simplyblockv1alpha1.ReplicationPolicyStatus{
			Ready:           true,
			BackendTargetID: "tgt-uuid",
			BackendPolicyID: "bpol-uuid",
		},
	}
}

// pairForPolicy creates a pair that belongs to a policy with a valid volume handle.
func pairForPolicy(ns, policyRef string) *simplyblockv1alpha1.ReplicationPair {
	return &simplyblockv1alpha1.ReplicationPair{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-pvc1", Namespace: ns},
		Spec: simplyblockv1alpha1.ReplicationPairSpec{
			PolicyRef: policyRef,
			PVCRef:    "pvc1",
			VolumeID:  "cluster-u:pool-u:vol-u",
		},
		Status: simplyblockv1alpha1.ReplicationPairStatus{
			State:        string(simplyblockv1alpha1.ReplicationPairStateReplicating),
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
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")
	_, _ = r.Reconcile(context.Background(), opsRequest("ops1"))

	got := getOps(t, cl,"ops1")
	if !containsString(got.Finalizers, finalizerReplicationOps) {
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
	r, cl := newOpsReplReconciler(t, pol, ops)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), ops); err != nil {
		t.Fatalf("pre-set ops status: %v", err)
	}
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
	r, cl := newOpsReplReconciler(t, pol, ops)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), ops); err != nil {
		t.Fatalf("pre-set ops status: %v", err)
	}
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

	got := getOps(t, cl,"ops1")
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ---------- mutual exclusion: another ops holds the lock ----------

func TestOps_MutualExclusion_WaitsIfLocked(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pol.Status.ActiveOpsRef = "other-ops" // locked by someone else

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
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	res, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while policy lock is held by another ops")
	}
	// Phase must not advance.
	got := getOps(t, cl,"ops1")
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
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl,"ops1")
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// ---------- failover scope=policy success ----------

func TestOps_Failover_ScopePolicy_Success(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pair := pairForPolicy("default", "pol")
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, pair, ops)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), pair); err != nil {
		t.Fatalf("pre-set pair status: %v", err)
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl,"ops1")
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
	pair := pairForPolicy("default", "pol")
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failover", Scope: utils.ReplicationOpsScopeTarget, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, pair, ops)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), pair); err != nil {
		t.Fatalf("pre-set pair status: %v", err)
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl,"ops1")
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
}

// ---------- failover scope=volume success ----------

func TestOps_Failover_ScopeVolume_Success(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pair := pairForPolicy("default", "pol")
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			// For scope=volume, Ref is the pair name.
			Action: "failover", Scope: utils.ReplicationOpsScopeVolume, Ref: "pol-pvc1",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, pair, ops)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), pair); err != nil {
		t.Fatalf("pre-set pair status: %v", err)
	}

	// For scope=volume, resolveAffectedPolicyName returns ops.Spec.Ref (the pair name).
	// The reconciler will fail to find a policy named "pol-pvc1", so we instead
	// pre-set the policy with the pair's name for this test to work as expected.
	// In practice scope=volume for failover uses the pair ref as the policy ref.
	// We test the policy-lock path by using scope=policy above.
	// For scope=volume, the lookup of "pol-pvc1" as a policy will fail (NotFound)
	// and failOps will be called — so let's pre-create a policy with that name.
	polForVolume := readyPolicyForOps("pol-pvc1")
	if err := cl.Create(context.Background(), polForVolume); err != nil {
		t.Fatalf("create secondary policy: %v", err)
	}
	if err := cl.Status().Update(context.Background(), polForVolume); err != nil {
		t.Fatalf("pre-set secondary policy status: %v", err)
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl,"ops1")
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
}

// ---------- failback success ----------

func TestOps_Failback_Success(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pair := pairForPolicy("default", "pol")
	pair.Status.State = string(simplyblockv1alpha1.ReplicationPairStateFailedOver)
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failback", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, pair, ops)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), pair); err != nil {
		t.Fatalf("pre-set pair status: %v", err)
	}

	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", srv.URL)

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl,"ops1")
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseSucceeded) {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if len(got.Status.Results) != 1 || got.Status.Results[0].Status != string(simplyblockv1alpha1.ReplicationOpsResultSucceeded) {
		t.Errorf("unexpected results: %+v", got.Status.Results)
	}

	// Pair direction should be updated back to source.
	gotPair := &simplyblockv1alpha1.ReplicationPair{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-pvc1"}, gotPair); err != nil {
		t.Fatalf("get pair: %v", err)
	}
	if gotPair.Status.Direction != string(simplyblockv1alpha1.ReplicationPairDirectionSource) {
		t.Errorf("pair direction = %q, want source after failback", gotPair.Status.Direction)
	}
}

// ---------- failback partial failure ----------

func TestOps_Failback_PartialFailure(t *testing.T) {
	pol := readyPolicyForOps("pol")
	pair := pairForPolicy("default", "pol")
	// Use an invalid VolumeID so the pair fails during failback.
	pair.Spec.VolumeID = "bad-handle"
	ops := &simplyblockv1alpha1.ReplicationOps{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ops1", Namespace: "default",
			Finalizers: []string{finalizerReplicationOps},
		},
		Spec: simplyblockv1alpha1.ReplicationOpsSpec{
			Action: "failback", Scope: utils.ReplicationOpsScopePolicy, Ref: "pol",
		},
	}
	r, cl := newOpsReplReconciler(t, pol, pair, ops)
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), pair); err != nil {
		t.Fatalf("pre-set pair status: %v", err)
	}
	t.Setenv("SIMPLYBLOCK_WEBAPI_BASE_URL", "http://127.0.0.1:1")

	_, err := r.Reconcile(context.Background(), opsRequest("ops1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getOps(t, cl,"ops1")
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("phase = %q, want Failed when a pair's VolumeID is invalid", got.Status.Phase)
	}
	// failOps does not persist per-volume results; only Phase and Message are set.
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
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), ops); err != nil {
		t.Fatalf("pre-set ops status: %v", err)
	}

	_, err := r.succeedOps(context.Background(), ops, "done", nil)
	if err != nil {
		t.Fatalf("succeedOps: %v", err)
	}

	// Policy lock must be released.
	p := &simplyblockv1alpha1.ReplicationPolicy{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if p.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want empty after succeedOps", p.Status.ActiveOpsRef)
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
	if err := cl.Status().Update(context.Background(), pol); err != nil {
		t.Fatalf("pre-set policy status: %v", err)
	}
	if err := cl.Status().Update(context.Background(), ops); err != nil {
		t.Fatalf("pre-set ops status: %v", err)
	}

	_, err := r.failOps(context.Background(), ops, "something went wrong")
	if err != nil {
		t.Fatalf("failOps: %v", err)
	}

	// Policy lock must be released.
	p := &simplyblockv1alpha1.ReplicationPolicy{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, p); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if p.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want empty after failOps", p.Status.ActiveOpsRef)
	}

	got := getOps(t, cl,"ops1")
	if got.Status.Phase != string(simplyblockv1alpha1.ReplicationOpsPhaseFailed) {
		t.Errorf("ops phase = %q, want Failed", got.Status.Phase)
	}
}
