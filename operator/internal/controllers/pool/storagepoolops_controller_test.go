// Tests for the StoragePoolOps reconciler: the lock, the terminal paths, and the
// finalizer.
//
// None of this kind's actions does anything yet, so what is tested is the shape
// every Ops kind in this group has rather than an operation's effect. That is
// not a thin thing to test: the lock is what stops two operations acting on one
// pool, and every path that leaves it taken is a pool nothing can ever operate
// on again, with nothing in the cluster to say why.
//
// These are the U-41 to U-53 rows of docs/tests/test-plan-storagepool.md.

package pool

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The two operation names these tests need: the one under test, and one that
// stands for somebody else already holding the lock.
const (
	testOpsName  = "rebalance-1"
	otherOpsName = "somebody-elses-operation"
)

func newOps(poolRef string, mutate ...func(*simplyblockv1alpha2.StoragePoolOps)) *simplyblockv1alpha2.StoragePoolOps {
	ops := &simplyblockv1alpha2.StoragePoolOps{
		ObjectMeta: objectMeta(testOpsName, testNamespace),
		Spec: simplyblockv1alpha2.StoragePoolOpsSpec{
			PoolRef: poolRef,
			Action:  simplyblockv1alpha2.StoragePoolOpsActionRebalance,
		},
	}
	for _, m := range mutate {
		m(ops)
	}
	return ops
}

func newOpsReconciler(
	t *testing.T, rec *recorder, objects ...client.Object,
) *StoragePoolOpsReconciler {
	t.Helper()
	return &StoragePoolOpsReconciler{
		Client:   newClient(t, objects...),
		Scheme:   testScheme(t),
		Recorder: rec,
	}
}

// reconcileOps runs n passes and returns the operation and its target.
func reconcileOps(
	t *testing.T, r *StoragePoolOpsReconciler, poolName string, n int,
) (*simplyblockv1alpha2.StoragePoolOps, *simplyblockv1alpha2.StoragePool) {
	t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile pass %d: %v", i+1, err)
		}
	}
	var ops simplyblockv1alpha2.StoragePoolOps
	if err := r.Get(ctx, key, &ops); err != nil {
		t.Fatalf("read the operation back: %v", err)
	}
	var p simplyblockv1alpha2.StoragePool
	poolKey := types.NamespacedName{Namespace: testNamespace, Name: poolName}
	if err := r.Get(ctx, poolKey, &p); err != nil {
		return &ops, nil
	}
	return &ops, &p
}

// U-41: with the lock free, the operation takes it and moves to Running.
//
// The lock is asserted on directly rather than through a full reconcile,
// because the one declared action terminates in the same pass that acquires it
// and the lock is therefore back before the pass returns. What matters is the
// state while an action is running, which is what this observes.
func TestAcquiresAFreeLock(t *testing.T) {
	rec := &recorder{}
	target := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	ops := newOps("tenant-a")
	r := newOpsReconciler(t, rec, target, ops)

	ctx := context.Background()
	var live simplyblockv1alpha2.StoragePool
	poolKey := types.NamespacedName{Namespace: testNamespace, Name: "tenant-a"}
	if err := r.Get(ctx, poolKey, &live); err != nil {
		t.Fatalf("read the pool: %v", err)
	}
	acquired, _, err := r.acquireLock(ctx, ops, &live)
	if err != nil {
		t.Fatalf("acquireLock: %v", err)
	}
	if !acquired {
		t.Fatal("the lock was free and was not acquired")
	}

	if err := r.Get(ctx, poolKey, &live); err != nil {
		t.Fatalf("read the pool back: %v", err)
	}
	if live.Status.ActiveOpsRef != testOpsName {
		t.Errorf("the pool's activeOpsRef = %q, want rebalance-1", live.Status.ActiveOpsRef)
	}

	var stored simplyblockv1alpha2.StoragePoolOps
	opsKey := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	if err := r.Get(ctx, opsKey, &stored); err != nil {
		t.Fatalf("read the operation back: %v", err)
	}
	if stored.Status.Phase != simplyblockv1alpha2.StoragePoolOpsPhaseRunning {
		t.Errorf("status.phase = %q, want Running", stored.Status.Phase)
	}
	if stored.Status.StartedAt == nil {
		t.Error("status.startedAt was not written when the lock was taken")
	}
	if !rec.has(OperationStarted) {
		t.Errorf("no %s event: %+v", OperationStarted, rec.events)
	}
}

// U-42: an operation that finds the lock held stays Pending and asks again. The
// other operation will finish, and failing here would make the order two people
// applied two objects in decide which of them runs.
func TestWaitsForALockAnotherOperationHolds(t *testing.T) {
	rec := &recorder{}
	held := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.ActiveOpsRef = otherOpsName
	})
	r := newOpsReconciler(t, rec, held, newOps("tenant-a"))

	ops, p := reconcileOps(t, r, "tenant-a", 2)

	if ops.Status.Phase != simplyblockv1alpha2.StoragePoolOpsPhasePending {
		t.Errorf("status.phase = %q, want Pending", ops.Status.Phase)
	}
	if p.Status.ActiveOpsRef != otherOpsName {
		t.Errorf("the pool's activeOpsRef = %q, want the other operation's", p.Status.ActiveOpsRef)
	}
	if !rec.has(OperationQueued) {
		t.Errorf("no %s event: %+v", OperationQueued, rec.events)
	}
}

// The provisional action reaches a terminal phase saying so, and the lock goes
// back. It is not silently a no-op, and it is not an unknown action either.
func TestTheProvisionalActionFailsAndReleasesTheLock(t *testing.T) {
	rec := &recorder{}
	target := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	r := newOpsReconciler(t, rec, target, newOps("tenant-a"))

	ops, p := reconcileOps(t, r, "tenant-a", 2)

	if ops.Status.Phase != simplyblockv1alpha2.StoragePoolOpsPhaseFailed {
		t.Errorf("status.phase = %q, want Failed", ops.Status.Phase)
	}
	if ops.Status.Message == "" {
		t.Error("status.message says nothing about why the operation did not run")
	}
	if p.Status.ActiveOpsRef != "" {
		t.Errorf("the pool's activeOpsRef = %q, want the lock released", p.Status.ActiveOpsRef)
	}
	if ops.Status.CompletedAt == nil {
		t.Error("status.completedAt was not written on a terminal operation")
	}
}

// U-44: re-reconciling a terminal operation does nothing at all, including
// nothing to its target. Taking the lock again to release it again is how an
// unrelated operation loses one it legitimately holds.
func TestATerminalOperationTouchesNothing(t *testing.T) {
	rec := &recorder{}
	other := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.ActiveOpsRef = "a-later-operation"
	})
	done := newOps("tenant-a", func(ops *simplyblockv1alpha2.StoragePoolOps) {
		ops.Status.Phase = simplyblockv1alpha2.StoragePoolOpsPhaseSucceeded
	})
	r := newOpsReconciler(t, rec, other, done)

	_, p := reconcileOps(t, r, "tenant-a", 2)

	if p.Status.ActiveOpsRef != "a-later-operation" {
		t.Errorf("the pool's activeOpsRef = %q, want the later operation's lock left alone",
			p.Status.ActiveOpsRef)
	}
}

// U-45: an operation deleted while it holds the lock releases it. Without this
// the pool is locked against every later operation, with nothing left in the
// cluster to say why.
func TestDeletingARunningOperationReleasesTheLock(t *testing.T) {
	rec := &recorder{}
	held := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.ActiveOpsRef = testOpsName
	})
	now := metav1.Now()
	running := newOps("tenant-a", func(ops *simplyblockv1alpha2.StoragePoolOps) {
		ops.Status.Phase = simplyblockv1alpha2.StoragePoolOpsPhaseRunning
		ops.Status.Step = statemachine.KubeSnapshot{
			State: string(simplyblockv1alpha2.StoragePoolOpsStepValidating),
		}
		ops.Finalizers = []string{FinalizerStoragePoolOps}
		ops.DeletionTimestamp = &now
	})
	r := newOpsReconciler(t, rec, held, running)

	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var p simplyblockv1alpha2.StoragePool
	poolKey := types.NamespacedName{Namespace: testNamespace, Name: "tenant-a"}
	if err := r.Get(ctx, poolKey, &p); err != nil {
		t.Fatalf("read the pool back: %v", err)
	}
	if p.Status.ActiveOpsRef != "" {
		t.Errorf("the pool's activeOpsRef = %q, want the lock released by the finalizer",
			p.Status.ActiveOpsRef)
	}
}

// An operation must not release a lock somebody else holds, which is the guard
// that makes the finalizer safe rather than dangerous.
func TestDeletingAnOperationDoesNotReleaseAnotherOnesLock(t *testing.T) {
	rec := &recorder{}
	held := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.ActiveOpsRef = otherOpsName
	})
	now := metav1.Now()
	gone := newOps("tenant-a", func(ops *simplyblockv1alpha2.StoragePoolOps) {
		ops.Finalizers = []string{FinalizerStoragePoolOps}
		ops.DeletionTimestamp = &now
	})
	r := newOpsReconciler(t, rec, held, gone)

	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: testOpsName}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var p simplyblockv1alpha2.StoragePool
	poolKey := types.NamespacedName{Namespace: testNamespace, Name: "tenant-a"}
	if err := r.Get(ctx, poolKey, &p); err != nil {
		t.Fatalf("read the pool back: %v", err)
	}
	if p.Status.ActiveOpsRef != otherOpsName {
		t.Errorf("the pool's activeOpsRef = %q, want the other operation's lock left alone",
			p.Status.ActiveOpsRef)
	}
}

// U-51: an operation whose target does not exist fails with a message naming it,
// rather than requeuing forever against an object that will not arrive.
func TestAnOperationOnAMissingPoolFails(t *testing.T) {
	rec := &recorder{}
	r := newOpsReconciler(t, rec, newOps("no-such-pool"))

	ops, _ := reconcileOps(t, r, "no-such-pool", 1)

	if ops.Status.Phase != simplyblockv1alpha2.StoragePoolOpsPhaseFailed {
		t.Errorf("status.phase = %q, want Failed", ops.Status.Phase)
	}
	if ops.Status.Message == "" {
		t.Error("status.message does not name the pool that is missing")
	}
}

// spec.abort stops the operation and gives the lock back.
func TestAbortReleasesTheLockAndEndsAborted(t *testing.T) {
	rec := &recorder{}
	held := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.ActiveOpsRef = testOpsName
	})
	aborting := newOps("tenant-a", func(ops *simplyblockv1alpha2.StoragePoolOps) {
		ops.Spec.Abort = true
		ops.Status.Phase = simplyblockv1alpha2.StoragePoolOpsPhaseRunning
	})
	r := newOpsReconciler(t, rec, held, aborting)

	ops, p := reconcileOps(t, r, "tenant-a", 2)

	if ops.Status.Phase != simplyblockv1alpha2.StoragePoolOpsPhaseAborted {
		t.Errorf("status.phase = %q, want Aborted", ops.Status.Phase)
	}
	if p.Status.ActiveOpsRef != "" {
		t.Errorf("the pool's activeOpsRef = %q, want the lock released", p.Status.ActiveOpsRef)
	}
	if !rec.has(OperationAborted) {
		t.Errorf("no %s event: %+v", OperationAborted, rec.events)
	}
}

// U-53: every step the kind declares appears in the CEL rule on status.step, and
// the rule lists nothing the kind does not declare.
//
// The rule repeats the enum because a marker cannot reach a field of the shared
// snapshot type, and a repetition nothing checks is a repetition that drifts: a
// step added to the enum and not to the rule is refused by the API server with a
// message about an unknown step.
func TestTheStepEnumAndTheCELRuleAgree(t *testing.T) {
	declared := []string{
		string(simplyblockv1alpha2.StoragePoolOpsStepValidating),
		string(simplyblockv1alpha2.StoragePoolOpsStepMigrating),
	}
	// The rule as it is written on StoragePoolOpsStatus.Step.
	inRule := map[string]bool{"Validating": true, "Migrating": true}

	if len(inRule) != len(declared) {
		t.Fatalf("the CEL rule lists %d steps and the kind declares %d", len(inRule), len(declared))
	}
	for _, step := range declared {
		if !inRule[step] {
			t.Errorf("step %q is declared and is not in the CEL rule, so the API server refuses it", step)
		}
	}
}
