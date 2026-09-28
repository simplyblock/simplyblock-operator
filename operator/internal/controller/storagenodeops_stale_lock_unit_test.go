// A StorageNode's activeOpsRef is only a lock while the op it names still
// runs. A StorageNodeOps has no finalizer: deleted mid-drain it vanishes
// without releasing, and every later op on the node was refused for ever
// ("another ops is active, requeuing", 2026-09-28). A finished op that never
// released is the same case. Both are stale and are taken over.

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

func acquireAgainst(t *testing.T, holder *simplyblockv1alpha1.StorageNodeOps) string {
	t.Helper()
	sn := newTestStorageNode("sn-1", opsTestNS, "sns", opsTestWorker, opsTestNodeUUID)
	sn.Status.ActiveOpsRef = opsTestOtherOps
	ops := newTestStorageNodeOps(opsTestOpsName, opsTestNS, "sn-1", "suspend")
	var r *StorageNodeOpsReconciler
	if holder != nil {
		r = newOpsReconciler(t, sn, ops, holder)
	} else {
		r = newOpsReconciler(t, sn, ops)
	}
	if _, err := r.acquireLock(context.Background(), ops, sn); err != nil {
		t.Fatalf("acquireLock: %v", err)
	}
	var updated simplyblockv1alpha1.StorageNode
	if err := r.Get(context.Background(), types.NamespacedName{Name: "sn-1", Namespace: opsTestNS}, &updated); err != nil {
		t.Fatalf("get StorageNode: %v", err)
	}
	return updated.Status.ActiveOpsRef
}

func TestAcquireLock_TakesOverALockHeldByADeletedOps(t *testing.T) {
	got := acquireAgainst(t, nil) // the holder named by activeOpsRef does not exist
	if got != opsTestOpsName {
		t.Errorf("activeOpsRef = %q, want %q: a deleted op must not hold the node for ever", got, opsTestOpsName)
	}
}

func TestAcquireLock_TakesOverALockHeldByAFinishedOps(t *testing.T) {
	for _, phase := range []simplyblockv1alpha1.StorageNodeOpsPhase{
		simplyblockv1alpha1.StorageNodeOpsPhaseSucceeded,
		simplyblockv1alpha1.StorageNodeOpsPhaseFailed,
	} {
		holder := newTestStorageNodeOps(opsTestOtherOps, opsTestNS, "sn-1", "suspend")
		holder.Status.Phase = phase
		got := acquireAgainst(t, holder)
		if got != opsTestOpsName {
			t.Errorf("%s holder: activeOpsRef = %q, want %q", phase, got, opsTestOpsName)
		}
	}
}

func TestAcquireLock_ALiveHolderKeepsTheLock(t *testing.T) {
	for _, phase := range []simplyblockv1alpha1.StorageNodeOpsPhase{
		simplyblockv1alpha1.StorageNodeOpsPhasePending,
		simplyblockv1alpha1.StorageNodeOpsPhaseRunning,
		"", // just created, no phase yet
	} {
		holder := newTestStorageNodeOps(opsTestOtherOps, opsTestNS, "sn-1", "suspend")
		holder.Status.Phase = phase
		got := acquireAgainst(t, holder)
		if got != opsTestOtherOps {
			t.Errorf("%q holder: activeOpsRef = %q, want it kept by %q", phase, got, opsTestOtherOps)
		}
	}
}
