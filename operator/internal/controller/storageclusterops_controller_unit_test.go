package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// ── helpers ───────────────────────────────────────────────────────────────────

const (
	scopsTestNS          = "test"
	scopsTestClusterName = "simplyblock-cluster"
	scopsTestClusterUUID = "bbbb0000-0000-0000-0000-000000000001"
	scopsTestOpsName     = "cops-1"
	scopsTestOtherOps    = "cops-other"
)

func newClusterOpsReconciler(t *testing.T, objects ...client.Object) *StorageClusterOpsReconciler {
	t.Helper()
	scheme := newTestScheme(t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
	)
	cl := newTestClient(t, scheme,
		[]client.Object{
			&simplyblockv1alpha1.StorageClusterOps{},
			&simplyblockv1alpha1.StorageCluster{},
		},
		objects...,
	)
	return &StorageClusterOpsReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(16),
	}
}

func newTestStorageCluster() *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: scopsTestClusterName, Namespace: scopsTestNS},
		Status: simplyblockv1alpha1.StorageClusterStatus{
			UUID: scopsTestClusterUUID,
		},
	}
}

func newTestStorageClusterOps(clusterRef, action string) *simplyblockv1alpha1.StorageClusterOps {
	return &simplyblockv1alpha1.StorageClusterOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:       scopsTestOpsName,
			Namespace:  scopsTestNS,
			Finalizers: []string{utils.FinalizerStorageClusterOps},
		},
		Spec: simplyblockv1alpha1.StorageClusterOpsSpec{
			ClusterRef: clusterRef,
			Action:     action,
		},
	}
}

// ── TestReconcile_TerminalPhases ──────────────────────────────────────────────

func TestStorageClusterOps_TerminalSucceeded_IsNoop(t *testing.T) {
	cluster := newTestStorageCluster()
	ops := newTestStorageClusterOps(scopsTestClusterName, "activate")
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseSucceeded
	r := newClusterOpsReconciler(t, cluster, ops)

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: scopsTestNS, Name: scopsTestOpsName}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue for terminal Succeeded phase")
	}
}

func TestStorageClusterOps_TerminalFailed_IsNoop(t *testing.T) {
	cluster := newTestStorageCluster()
	ops := newTestStorageClusterOps(scopsTestClusterName, "expand")
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseFailed
	r := newClusterOpsReconciler(t, cluster, ops)

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: scopsTestNS, Name: scopsTestOpsName}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue for terminal Failed phase")
	}
}

// ── TestReconcile_ClusterNotFound ─────────────────────────────────────────────

func TestStorageClusterOps_ClusterNotFound_Fails(t *testing.T) {
	ops := newTestStorageClusterOps("missing-cluster", "activate")
	r := newClusterOpsReconciler(t, ops)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: scopsTestNS, Name: scopsTestOpsName}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageClusterOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestOpsName, Namespace: scopsTestNS}, &updated)
	if updated.Status.Phase != simplyblockv1alpha1.StorageClusterOpsPhaseFailed {
		t.Errorf("phase: got %q want Failed", updated.Status.Phase)
	}
	if updated.Status.Message == "" {
		t.Error("expected non-empty failure message")
	}
}

// ── TestMutualExclusion ───────────────────────────────────────────────────────

func TestStorageClusterOps_RequeuesWhenAnotherOpsActive(t *testing.T) {
	cluster := newTestStorageCluster()
	cluster.Status.ActiveOpsRef = scopsTestOtherOps

	ops := newTestStorageClusterOps(scopsTestClusterName, "activate")
	r := newClusterOpsReconciler(t, cluster, ops)

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: scopsTestNS, Name: scopsTestOpsName}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter when another ops is active")
	}

	// cluster.activeOpsRef must NOT be changed.
	var updatedCluster simplyblockv1alpha1.StorageCluster
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestClusterName, Namespace: scopsTestNS}, &updatedCluster)
	if updatedCluster.Status.ActiveOpsRef != scopsTestOtherOps {
		t.Errorf("activeOpsRef should not change: got %q", updatedCluster.Status.ActiveOpsRef)
	}
}

func TestStorageClusterOps_AcquiresLockAndTransitionsOutOfPending(t *testing.T) {
	cluster := newTestStorageCluster()
	ops := newTestStorageClusterOps(scopsTestClusterName, "shutdown")
	r := newClusterOpsReconciler(t, cluster, ops)

	// shutdown POSTs to the backend — fails with no real API, so the ops ends up
	// Failed. What we verify is that it moved out of Pending (lock was acquired,
	// dispatch ran) and that activeOpsRef is cleared again (failOps released it).
	r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: scopsTestNS, Name: scopsTestOpsName}}) //nolint:errcheck

	var updatedOps simplyblockv1alpha1.StorageClusterOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestOpsName, Namespace: scopsTestNS}, &updatedOps)
	if updatedOps.Status.Phase == simplyblockv1alpha1.StorageClusterOpsPhasePending || updatedOps.Status.Phase == "" {
		t.Errorf("ops should have left Pending phase, got %q", updatedOps.Status.Phase)
	}

	// Lock must be released after the reconcile completes (either success or failure).
	var updatedCluster simplyblockv1alpha1.StorageCluster
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestClusterName, Namespace: scopsTestNS}, &updatedCluster)
	if updatedCluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef should be cleared after reconcile, got %q", updatedCluster.Status.ActiveOpsRef)
	}
}

// ── TestSucceedOps ────────────────────────────────────────────────────────────

func TestStorageClusterOps_SucceedOps_SetsPhaseAndClearsLock(t *testing.T) {
	cluster := newTestStorageCluster()
	cluster.Status.ActiveOpsRef = scopsTestOpsName

	ops := newTestStorageClusterOps(scopsTestClusterName, "activate")
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseRunning
	r := newClusterOpsReconciler(t, cluster, ops)

	result, err := r.succeedOps(context.Background(), ops, cluster, "activated successfully")
	if err != nil {
		t.Fatalf("succeedOps returned error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue after success")
	}

	var updatedOps simplyblockv1alpha1.StorageClusterOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestOpsName, Namespace: scopsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageClusterOpsPhaseSucceeded {
		t.Errorf("phase: got %q want Succeeded", updatedOps.Status.Phase)
	}
	if updatedOps.Status.Message != "activated successfully" {
		t.Errorf("message: got %q", updatedOps.Status.Message)
	}
	if updatedOps.Status.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	var updatedCluster simplyblockv1alpha1.StorageCluster
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestClusterName, Namespace: scopsTestNS}, &updatedCluster)
	if updatedCluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef should be cleared, got %q", updatedCluster.Status.ActiveOpsRef)
	}
}

// ── TestFailOps ───────────────────────────────────────────────────────────────

func TestStorageClusterOps_FailOps_SetsPhaseAndClearsLock(t *testing.T) {
	cluster := newTestStorageCluster()
	cluster.Status.ActiveOpsRef = scopsTestOpsName

	ops := newTestStorageClusterOps(scopsTestClusterName, "expand")
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseRunning
	r := newClusterOpsReconciler(t, cluster, ops)

	result, err := r.failOps(context.Background(), ops, cluster, "expand POST failed: status 500")
	if err != nil {
		t.Fatalf("failOps returned error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue after failure")
	}

	var updatedOps simplyblockv1alpha1.StorageClusterOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestOpsName, Namespace: scopsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageClusterOpsPhaseFailed {
		t.Errorf("phase: got %q want Failed", updatedOps.Status.Phase)
	}
	if updatedOps.Status.Message != "expand POST failed: status 500" {
		t.Errorf("message: got %q", updatedOps.Status.Message)
	}
	if updatedOps.Status.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	var updatedCluster simplyblockv1alpha1.StorageCluster
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestClusterName, Namespace: scopsTestNS}, &updatedCluster)
	if updatedCluster.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef should be cleared after failure, got %q", updatedCluster.Status.ActiveOpsRef)
	}
}

func TestStorageClusterOps_FailOps_NilCluster_DoesNotPanic(t *testing.T) {
	ops := newTestStorageClusterOps(scopsTestClusterName, "activate")
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseRunning
	r := newClusterOpsReconciler(t, ops)

	_, err := r.failOps(context.Background(), ops, nil, "cluster not found")
	if err != nil {
		t.Fatalf("failOps with nil cluster returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageClusterOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestOpsName, Namespace: scopsTestNS}, &updated)
	if updated.Status.Phase != simplyblockv1alpha1.StorageClusterOpsPhaseFailed {
		t.Errorf("phase: got %q want Failed", updated.Status.Phase)
	}
}

// ── TestReleaseClusterLock ────────────────────────────────────────────────────

func TestStorageClusterOps_ReleaseLock_OnlyClearsIfOwner(t *testing.T) {
	cluster := newTestStorageCluster()
	cluster.Status.ActiveOpsRef = scopsTestOtherOps

	ops := newTestStorageClusterOps(scopsTestClusterName, "activate")
	r := newClusterOpsReconciler(t, cluster, ops)

	r.releaseClusterLock(context.Background(), ops, cluster)

	var updated simplyblockv1alpha1.StorageCluster
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestClusterName, Namespace: scopsTestNS}, &updated)
	if updated.Status.ActiveOpsRef != scopsTestOtherOps {
		t.Error("releaseClusterLock should not clear a lock it does not own")
	}
}

func TestStorageClusterOps_ReleaseLock_NilCluster_DoesNotPanic(t *testing.T) {
	ops := newTestStorageClusterOps(scopsTestClusterName, "activate")
	r := newClusterOpsReconciler(t, ops)

	// Should be a no-op and not panic.
	r.releaseClusterLock(context.Background(), ops, nil)
}

// ── TestUnknownAction ─────────────────────────────────────────────────────────

func TestStorageClusterOps_UnknownAction_Fails(t *testing.T) {
	cluster := newTestStorageCluster()
	ops := newTestStorageClusterOps(scopsTestClusterName, "bogus-action")
	r := newClusterOpsReconciler(t, cluster, ops)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: scopsTestNS, Name: scopsTestOpsName}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageClusterOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestOpsName, Namespace: scopsTestNS}, &updated)
	if updated.Status.Phase != simplyblockv1alpha1.StorageClusterOpsPhaseFailed {
		t.Errorf("phase: got %q want Failed for unknown action", updated.Status.Phase)
	}
}

// ── TestNodeRollingRestart_Initialises ───────────────────────────────────────

func TestStorageClusterOps_NodeRollingRestart_Initialises(t *testing.T) {
	cluster := newTestStorageCluster()
	ops := newTestStorageClusterOps(scopsTestClusterName, "node-rolling-restart")
	r := newClusterOpsReconciler(t, cluster, ops)

	// First reconcile: the state machine sets ops.Status.Triggered=true and
	// requeues. No backend is available so it can't list nodes yet.
	r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: scopsTestNS, Name: scopsTestOpsName}}) //nolint:errcheck

	var updatedOps simplyblockv1alpha1.StorageClusterOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestOpsName, Namespace: scopsTestNS}, &updatedOps)
	if !updatedOps.Status.Triggered {
		t.Error("ops.status.triggered should be true after first reconcile")
	}
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageClusterOpsPhaseRunning {
		t.Errorf("ops phase: got %q want Running", updatedOps.Status.Phase)
	}
}

// ── TestReconcileActivate_FailureDomainReadinessGate ─────────────────────────
//
// Ported from simplyblockstoragecluster_controller_unit_test.go's
// TestReconcileActivateWaitsForFailureDomainReadiness /
// TestReconcileActivateProceedsOnceFailureDomainsAreReady when reconcileActivate
// moved here with the ClusterOps split (#397). clusterFailureDomainHosts /
// fdActivationDomainCountViolation themselves still live in
// simplyblockstoragecluster_controller.go and are exercised directly by
// TestFdActivationDomainCountViolation / TestClusterFailureDomainHosts there;
// these two only cover the gate's wiring into the new reconcileActivate.

func TestReconcileActivateWaitsForFailureDomainReadiness(t *testing.T) {
	fdv := func(v int32) *int32 { return &v }

	cluster := newTestStorageCluster()
	cluster.Spec.EnableFailureDomains = ptr.To(true)
	cluster.Spec.StripeSpec = &simplyblockv1alpha1.StripeSpec{ParityChunks: ptr.To(int32(2))}

	// Only 2 distinct domains for npcs=2 -- must NOT be allowed through
	// (requires npcs+2 = 4).
	nodeSet := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set-fd-wait", Namespace: scopsTestNS},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: scopsTestClusterName},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "w0", MgmtIp: "10.0.0.1", FailureDomain: fdv(0)},
				{Hostname: "w1", MgmtIp: "10.0.0.2", FailureDomain: fdv(1)},
			},
		},
	}

	ops := newTestStorageClusterOps(scopsTestClusterName, "activate")
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseRunning
	r := newClusterOpsReconciler(t, cluster, ops, nodeSet)

	res, err := r.reconcileActivate(context.Background(), ops, cluster)
	if err != nil {
		t.Fatalf("reconcileActivate returned error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected a requeue while waiting on failure-domain readiness")
	}
	if ops.Status.Triggered {
		t.Fatalf("expected ops to stay untriggered while failure domains aren't ready")
	}
	if ops.Status.Phase == simplyblockv1alpha1.StorageClusterOpsPhaseFailed {
		t.Fatalf("expected ops to stay out of Failed while waiting on failure-domain readiness")
	}
}

func TestReconcileActivateProceedsOnceFailureDomainsAreReady(t *testing.T) {
	fdv := func(v int32) *int32 { return &v }

	cluster := newTestStorageCluster()
	cluster.Spec.EnableFailureDomains = ptr.To(true)
	cluster.Spec.StripeSpec = &simplyblockv1alpha1.StripeSpec{ParityChunks: ptr.To(int32(2))}

	// 4 distinct, equally-sized domains for npcs=2 -- satisfies npcs+2 = 4,
	// so the gate must let this through to the real activate attempt.
	nodeSet := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set-fd-ready", Namespace: scopsTestNS},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: scopsTestClusterName},
		Status: simplyblockv1alpha1.StorageNodeSetStatus{
			Nodes: []simplyblockv1alpha1.NodeStatus{
				{Hostname: "w0", MgmtIp: "10.0.0.1", FailureDomain: fdv(0)},
				{Hostname: "w1", MgmtIp: "10.0.0.2", FailureDomain: fdv(1)},
				{Hostname: "w2", MgmtIp: "10.0.0.3", FailureDomain: fdv(2)},
				{Hostname: "w3", MgmtIp: "10.0.0.4", FailureDomain: fdv(3)},
			},
		},
	}

	ops := newTestStorageClusterOps(scopsTestClusterName, "activate")
	ops.Status.Phase = simplyblockv1alpha1.StorageClusterOpsPhaseRunning
	r := newClusterOpsReconciler(t, cluster, ops, nodeSet)

	// No real webapi backend is reachable from this unit test, so once the
	// FD gate lets the call through it fails resolving the cluster UUID --
	// same accepted pattern as TestStorageClusterOps_AcquiresLockAndTransitionsOutOfPending.
	// What's under test here is that it got PAST the gate (Failed from the
	// real attempt), not stuck re-requeuing on the readiness check.
	_, err := r.reconcileActivate(context.Background(), ops, cluster)
	if err != nil {
		t.Fatalf("reconcileActivate returned error: %v", err)
	}

	var updatedOps simplyblockv1alpha1.StorageClusterOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: scopsTestOpsName, Namespace: scopsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageClusterOpsPhaseFailed {
		t.Fatalf("expected the call to proceed past the FD gate to the (failing, no backend) "+
			"activate attempt, got phase %q", updatedOps.Status.Phase)
	}
}
