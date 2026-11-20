// The node's lock, and the three paths that let go of it.
//
// One operation touches one node at a time, and the lock that guarantees it is a
// field on the node rather than anything this controller holds in memory: two
// operator replicas, or one replica across a restart, have to reach the same
// answer. Acquisition is an optimistic-lock patch for the same reason — two
// operations can both read an empty field and both conclude the lock is free, and
// only one of them can have written it.
//
// Releasing is the half that goes wrong quietly. A release that does not check
// ownership clears a lock somebody else now holds, which is worse than never
// releasing: two operations then run against one node with neither knowing. And a
// lock nothing releases leaves a node held by an operation that has finished or
// has been deleted, which is why the release happens on the terminal transition,
// again on every pass of a terminal operation, and again from the finalizer.
//
// design-storagenode.md §7.1 and §11.

package node

import (
	"context"
	"testing"

	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// lockHolder is what the node currently says holds it.
func lockHolder(t *testing.T, apiClient client.Client) string {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	return node.Status.ActiveOpsRef
}

// operationRead is the operation as the API server now holds it.
func operationRead(
	t *testing.T, apiClient client.Client, name string,
) *simplyblockv1alpha2.StorageNodeOps {
	t.Helper()
	var ops simplyblockv1alpha2.StorageNodeOps
	key := client.ObjectKey{Namespace: opsNamespace, Name: name}
	if err := apiClient.Get(context.Background(), key, &ops); err != nil {
		t.Fatalf("reading the operation: %v", err)
	}
	return &ops
}

// held marks the node as locked by the named operation.
func lockedBy(t *testing.T, apiClient client.Client, name string) {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	node.Status.ActiveOpsRef = name
	if err := apiClient.Status().Update(context.Background(), &node); err != nil {
		t.Fatalf("seeding the lock: %v", err)
	}
}

// Taking the lock is what moves the operation out of Pending, and both halves
// have to be visible: the node says who holds it, and the operation says it is
// running and since when.
func TestTakingTheLockIsWhatStartsTheOperation(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)

	acquired, err := r.acquireLock(context.Background(), ops)
	if err != nil {
		t.Fatalf("acquiring the lock: %v", err)
	}
	if !acquired {
		t.Fatal("the operation did not take a lock nothing else holds")
	}
	if holder := lockHolder(t, apiClient); holder != "a-suspend" {
		t.Errorf("the node is held by %q, want the operation that took it", holder)
	}

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if got.Status.StartedAt == nil {
		t.Error("nothing records when the operation started, which its duration is measured from")
	}
}

// An operation whose node is held waits, and says so. It must not take the lock,
// and it must not fail: the holder finishing is what wakes it.
func TestAnOperationWaitsForTheOneHoldingTheNode(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	lockedBy(t, apiClient, "somebody-elses-operation")

	acquired, err := r.acquireLock(context.Background(), ops)
	if err != nil {
		t.Fatalf("acquiring the lock: %v", err)
	}
	if acquired {
		t.Fatal("the operation took a lock another operation holds")
	}
	if holder := lockHolder(t, apiClient); holder != "somebody-elses-operation" {
		t.Errorf("the lock is now held by %q, and the holder was overwritten", holder)
	}

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhasePending {
		t.Errorf("phase = %q, want Pending: a queued operation has not started", got.Status.Phase)
	}
	if !announcedReason(r, OperationQueued) {
		t.Error("nothing announced the wait, which is what tells it from a stalled controller")
	}
}

// The holder re-reading its own lock still holds it. Every pass of a running
// operation goes through this, so it has to be a no-op rather than a second
// acquisition.
func TestTheHolderKeepsItsOwnLock(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	lockedBy(t, apiClient, "a-suspend")

	acquired, err := r.acquireLock(context.Background(), ops)
	if err != nil {
		t.Fatalf("acquiring the lock: %v", err)
	}
	if !acquired {
		t.Error("the operation lost a lock it already held")
	}
}

// An operation against a node that is not there cannot be run, and holding it
// open would leave a record nobody can resolve.
func TestAnOperationAgainstAMissingNodeFails(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	ops.Spec.NodeRef = "no-such-node"
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)

	acquired, err := r.acquireLock(context.Background(), ops)
	if err != nil {
		t.Fatalf("acquiring the lock: %v", err)
	}
	if acquired {
		t.Fatal("the operation claims to hold a node that does not exist")
	}

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.Message == "" {
		t.Error("the failure says nothing about which node is missing")
	}
}

// A release only clears a lock that still names this operation. A pass that
// started before the lock changed hands would otherwise unlock a node somebody
// else is working on.
func TestALateReleaseDoesNotUnlockSomebodyElsesNode(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	lockedBy(t, apiClient, "the-operation-that-took-it-next")

	if err := r.releaseLock(context.Background(), ops); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}
	if holder := lockHolder(t, apiClient); holder != "the-operation-that-took-it-next" {
		t.Errorf("the lock is now %q, and an operation cleared one it did not hold", holder)
	}
}

// Finishing writes the outcome and lets the node go, in that order: the release
// is what admits the next operation, and it must not admit one before the record
// of this operation is durable.
func TestFinishingWritesTheOutcomeAndLetsTheNodeGo(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	lockedBy(t, apiClient, "a-suspend")

	_, err := r.finish(context.Background(), ops,
		simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded, "the node was suspended")
	if err != nil {
		t.Fatalf("finishing: %v", err)
	}

	got := operationRead(t, apiClient, "a-suspend")
	if got.Status.Phase != simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if got.Status.CompletedAt == nil {
		t.Error("nothing records when the operation ended")
	}
	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is still held by %q after the operation finished", holder)
	}
}

// A terminal operation is a record, and a record does nothing — except let go of
// a lock it still holds. That covers the pass that crashed between persisting
// the phase and clearing the field, which would otherwise leave the node held by
// a finished operation for good.
func TestATerminalOperationStillReleasesALockItLeftBehind(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	ops.Finalizers = []string{OpsFinalizer}
	ops.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	lockedBy(t, apiClient, "a-suspend")

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ops),
	}); err != nil {
		t.Fatalf("reconciling the finished operation: %v", err)
	}

	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is held by %q, which has already finished", holder)
	}
}

// Deleting a running operation releases the node before the object goes.
// Without it, a `kubectl delete` mid-drain would leave the node locked by an
// object that no longer exists, and nothing would ever unlock it.
func TestDeletingAnOperationUnlocksTheNodeFirst(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	ops.Finalizers = []string{OpsFinalizer}
	ops.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseRunning
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)
	lockedBy(t, apiClient, "a-suspend")

	if err := apiClient.Delete(context.Background(), ops); err != nil {
		t.Fatalf("deleting the operation: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ops),
	}); err != nil {
		t.Fatalf("reconciling the deleted operation: %v", err)
	}

	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node is held by %q, an operation that has been deleted", holder)
	}
	var gone simplyblockv1alpha2.StorageNodeOps
	err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(ops), &gone)
	if err == nil && controllerutil.ContainsFinalizer(&gone, OpsFinalizer) {
		t.Error("the finalizer is still there, so the object is held by its own teardown")
	}
}

// The first pass of a new operation puts the finalizer on, because the lock it
// is about to take has to be released even if the object is deleted mid-flight.
func TestAnOperationTakesItsFinalizerBeforeItTakesAnything(t *testing.T) {
	ops := anOperation("a-suspend", simplyblockv1alpha2.StorageNodeOpsActionSuspend)
	r, apiClient := anOpsWorld(t, aControlPlane(), ops)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ops),
	}); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	got := operationRead(t, apiClient, "a-suspend")
	if !controllerutil.ContainsFinalizer(got, OpsFinalizer) {
		t.Error("the operation carries no finalizer, so a delete mid-flight would strand the lock")
	}
	if holder := lockHolder(t, apiClient); holder != "" {
		t.Errorf("the node was locked by %q on the pass that only added the finalizer", holder)
	}
}

// announcedReason reports whether the operation raised one reason, which is what
// a queued operation owes whoever is wondering why nothing is happening.
func announcedReason(r *StorageNodeOpsReconciler, reason string) bool {
	return announced(r.Recorder.(*events.FakeRecorder), reason)
}
