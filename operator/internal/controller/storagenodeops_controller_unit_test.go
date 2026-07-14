package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

// ── helpers ───────────────────────────────────────────────────────────────────

const (
	opsTestNS      = "test"
	opsTestCluster = "cluster-a"
	opsTestWorker  = "worker-1.example.com"
	opsTestNodeUUID = "aaaa0000-0000-0000-0000-000000000001"
)

func newOpsReconciler(t *testing.T, objects ...client.Object) *StorageNodeOpsReconciler {
	t.Helper()
	scheme := newTestScheme(t,
		simplyblockv1alpha1.AddToScheme,
		corev1.AddToScheme,
	)
	cl := newTestClient(t, scheme,
		[]client.Object{
			&simplyblockv1alpha1.StorageNode{},
			&simplyblockv1alpha1.StorageNodeOps{},
			&simplyblockv1alpha1.StorageNodeSet{},
			&simplyblockv1alpha1.StorageCluster{},
			&simplyblockv1alpha1.VolumeMigration{},
		},
		objects...,
	)
	return &StorageNodeOpsReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(16),
	}
}

func newTestStorageNode(name, ns, snsRef, worker, uuid string) *simplyblockv1alpha1.StorageNode {
	sn := &simplyblockv1alpha1.StorageNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: simplyblockv1alpha1.StorageNodeSpec{
			StorageNodeSetRef: snsRef,
			WorkerNode:        worker,
		},
	}
	sn.Status.UUID = uuid
	return sn
}

func newTestStorageNodeOps(name, ns, snRef, action string) *simplyblockv1alpha1.StorageNodeOps {
	return &simplyblockv1alpha1.StorageNodeOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: simplyblockv1alpha1.StorageNodeOpsSpec{
			StorageNodeRef: snRef,
			Action:         action,
		},
	}
}

// ── TestAcquireLock ───────────────────────────────────────────────────────────

func TestAcquireLock_SetsActiveOpsRefAndTransitionsToRunning(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps("ops-1", opsTestNS, "sn-1", "suspend")
	r := newOpsReconciler(t, sn, ops)

	_, err := r.acquireLock(context.Background(), ops, sn)
	if err != nil {
		t.Fatalf("acquireLock returned error: %v", err)
	}

	// Check StorageNode.status.activeOpsRef was set.
	var updatedSN simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updatedSN)
	if updatedSN.Status.ActiveOpsRef != "ops-1" {
		t.Errorf("activeOpsRef: got %q want ops-1", updatedSN.Status.ActiveOpsRef)
	}

	// Check ops phase was set to Running.
	var updatedOps simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: "ops-1", Namespace: opsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseRunning {
		t.Errorf("phase: got %q want Running", updatedOps.Status.Phase)
	}
}

func TestAcquireLock_RequeuesWhenAnotherOpsActive(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = "ops-other"
	ops := newTestStorageNodeOps("ops-1", opsTestNS, "sn-1", "suspend")
	r := newOpsReconciler(t, sn, ops)

	result, err := r.acquireLock(context.Background(), ops, sn)
	if err != nil {
		t.Fatalf("acquireLock returned error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when another ops is active")
	}

	// StorageNode.activeOpsRef must NOT be changed.
	var updatedSN simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updatedSN)
	if updatedSN.Status.ActiveOpsRef != "ops-other" {
		t.Errorf("activeOpsRef should not change: got %q", updatedSN.Status.ActiveOpsRef)
	}
}

func TestAcquireLock_RemoveDrainSetsValidatingSubPhase(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	ops := newTestStorageNodeOps("ops-drain", opsTestNS, "sn-1", "remove")
	r := newOpsReconciler(t, sn, ops)

	_, err := r.acquireLock(context.Background(), ops, sn)
	if err != nil {
		t.Fatalf("acquireLock returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: "ops-drain", Namespace: opsTestNS}, &updated)
	if updated.Status.SubPhase != simplyblockv1alpha1.StorageNodeOpsSubPhaseValidating {
		t.Errorf("subPhase: got %q want Validating", updated.Status.SubPhase)
	}
}

// ── TestSucceedOps ────────────────────────────────────────────────────────────

func TestSucceedOps_SetsPhaseAndClearsLock(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = "ops-1"
	ops := newTestStorageNodeOps("ops-1", opsTestNS, "sn-1", "suspend")
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	r := newOpsReconciler(t, sn, ops)

	_, err := r.succeedOps(context.Background(), ops, sn)
	if err != nil {
		t.Fatalf("succeedOps returned error: %v", err)
	}

	var updatedOps simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: "ops-1", Namespace: opsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded {
		t.Errorf("phase: got %q want Succeeded", updatedOps.Status.Phase)
	}
	if updatedOps.Status.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	var updatedSN simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updatedSN)
	if updatedSN.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef should be cleared, got %q", updatedSN.Status.ActiveOpsRef)
	}
}

// ── TestFailOps ───────────────────────────────────────────────────────────────

func TestFailOps_SetsPhaseAndClearsLock(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = "ops-1"
	ops := newTestStorageNodeOps("ops-1", opsTestNS, "sn-1", "suspend")
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	r := newOpsReconciler(t, sn, ops)

	_, err := r.failOps(context.Background(), ops, "something went wrong")
	if err != nil {
		t.Fatalf("failOps returned error: %v", err)
	}

	var updatedOps simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: "ops-1", Namespace: opsTestNS}, &updatedOps)
	if updatedOps.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("phase: got %q want Failed", updatedOps.Status.Phase)
	}
	if updatedOps.Status.Message != "something went wrong" {
		t.Errorf("message: got %q", updatedOps.Status.Message)
	}

	var updatedSN simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updatedSN)
	if updatedSN.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef should be cleared after failure, got %q", updatedSN.Status.ActiveOpsRef)
	}
}

// ── TestReleaseLock ───────────────────────────────────────────────────────────

func TestReleaseLock_OnlyClearsIfOwner(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = "ops-other"
	r := newOpsReconciler(t, sn)

	// Releasing with a different name should be a no-op.
	if err := r.releaseLock(context.Background(), sn, "ops-1"); err != nil {
		t.Fatalf("releaseLock returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNode
	_ = r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updated)
	if updated.Status.ActiveOpsRef != "ops-other" {
		t.Error("releaseLock should not clear a lock it does not own")
	}
}

// ── TestAdvanceSubPhase ───────────────────────────────────────────────────────

func TestAdvanceSubPhase_UpdatesSubPhaseAndResetsTrigger(t *testing.T) {
	ops := newTestStorageNodeOps("ops-drain", opsTestNS, "sn-1", "remove")
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	ops.Status.SubPhase = simplyblockv1alpha1.StorageNodeOpsSubPhaseValidating
	ops.Status.Triggered = true
	r := newOpsReconciler(t, ops)

	_, err := r.advanceSubPhase(context.Background(), ops, simplyblockv1alpha1.StorageNodeOpsSubPhaseSuspending)
	if err != nil {
		t.Fatalf("advanceSubPhase returned error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: "ops-drain", Namespace: opsTestNS}, &updated)
	if updated.Status.SubPhase != simplyblockv1alpha1.StorageNodeOpsSubPhaseSuspending {
		t.Errorf("subPhase: got %q want Suspending", updated.Status.SubPhase)
	}
	if updated.Status.Triggered {
		t.Error("Triggered should be reset to false on phase advance")
	}
}

// ── TestDispatch ──────────────────────────────────────────────────────────────

func TestDispatch_UnknownActionFails(t *testing.T) {
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sns := &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sns", Namespace: opsTestNS},
		Spec:       simplyblockv1alpha1.StorageNodeSetSpec{ClusterName: opsTestCluster},
	}
	ops := newTestStorageNodeOps("ops-1", opsTestNS, "sn-1", "bogus-action")
	ops.Status.Phase = simplyblockv1alpha1.StorageNodeOpsPhaseRunning
	r := newOpsReconciler(t, sn, sns, ops)

	_, err := r.dispatch(context.Background(), ops, sn, sns, "cluster-uuid", nil)
	if err != nil {
		t.Fatalf("dispatch returned unexpected error: %v", err)
	}

	var updated simplyblockv1alpha1.StorageNodeOps
	_ = r.Get(context.Background(), types.NamespacedName{Name: "ops-1", Namespace: opsTestNS}, &updated)
	if updated.Status.Phase != simplyblockv1alpha1.StorageNodeOpsPhaseFailed {
		t.Errorf("expected Failed for unknown action, got %q", updated.Status.Phase)
	}
}

// ── TestResolveOpsSystemVolumeFilter ─────────────────────────────────────────

func TestResolveOpsSystemVolumeFilter_UsesDefaultWhenNoDrain(t *testing.T) {
	ops := newTestStorageNodeOps("ops-1", opsTestNS, "sn-1", "remove")
	r := newOpsReconciler(t, ops)

	re, err := r.resolveOpsSystemVolumeFilter(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Default pattern matches sb-fio-baseline-* names.
	if !re.MatchString("sb-fio-baseline-read") {
		t.Error("default filter should match sb-fio-baseline-read")
	}
	if re.MatchString("user-volume") {
		t.Error("default filter should not match user volumes")
	}
}

func TestResolveOpsSystemVolumeFilter_UsesCustomPattern(t *testing.T) {
	custom := "^bench-.*"
	ops := newTestStorageNodeOps("ops-1", opsTestNS, "sn-1", "remove")
	ops.Spec.Drain = &simplyblockv1alpha1.DrainOpsSpec{SystemVolumeFilterRegex: &custom}
	r := newOpsReconciler(t, ops)

	re, err := r.resolveOpsSystemVolumeFilter(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !re.MatchString("bench-read") {
		t.Error("custom filter should match bench-read")
	}
	if re.MatchString("sb-fio-baseline-read") {
		t.Error("custom filter should not match sb-fio-baseline-read")
	}
}

func TestResolveOpsSystemVolumeFilter_InvalidPatternReturnsError(t *testing.T) {
	bad := "["
	ops := newTestStorageNodeOps("ops-1", opsTestNS, "sn-1", "remove")
	ops.Spec.Drain = &simplyblockv1alpha1.DrainOpsSpec{SystemVolumeFilterRegex: &bad}
	r := newOpsReconciler(t, ops)

	_, err := r.resolveOpsSystemVolumeFilter(ops)
	if err == nil {
		t.Fatal("expected error for invalid regex pattern")
	}
}
