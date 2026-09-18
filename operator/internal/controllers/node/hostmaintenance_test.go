// Surviving a Kubernetes worker being drained.
//
// The window runs backward from an ordinary disruption budget: one that allows no
// disruption at all is created *before* the backend node is shut down, so
// `kubectl drain` blocks on it while the node is taken down gracefully, and
// relaxing it is what lets the drain proceed. The ordering is the whole
// mechanism — a drain that reaches the pod before the budget exists kills the
// SPDK process under a running backend node, which is exactly what the action
// exists to prevent.
//
// The gate in front of it is cluster-wide and counted by worker rather than by
// operation, because two nodes on one host go into maintenance together and the
// pair is one worker's worth of unavailability.
//
// design-storagenode.md §10.

package node

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aWindow is a maintenance window on the fixture's node.
func aWindow(name string) *simplyblockv1alpha2.StorageNodeOps {
	return anOperation(name, simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance)
}

// aRunningWindow is somebody else's window, past Holding and therefore holding a
// slot, against a node of its own on the given worker.
func aRunningWindow(
	name, nodeName, worker string,
) (*simplyblockv1alpha2.StorageNodeOps, *simplyblockv1alpha2.StorageNode) {
	ops := aWindow(name)
	ops.Spec.NodeRef = nodeName
	ops.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseRunning
	ops.Status.Step = kubeStep(stepShuttingDown)

	node := anOpsNode()
	node.Name = nodeName
	node.Spec.WorkerNode = worker
	return ops, node
}

// allowing states how many workers the cluster can afford to have down at once,
// which is the smaller of what it asks for and the fault tolerance the control
// plane reports.
func allowing(t *testing.T, apiClient client.Client, windows int32) {
	t.Helper()
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsCluster}
	if err := apiClient.Get(context.Background(), key, &cluster); err != nil {
		t.Fatalf("reading the cluster: %v", err)
	}
	cluster.Status.MaxConcurrentWorkerRestarts = ptr.To(windows)
	if err := apiClient.Status().Update(context.Background(), &cluster); err != nil {
		t.Fatalf("stating the cluster's concurrency: %v", err)
	}
}

// held runs the gate and reports whether it held this window back.
func held(t *testing.T, r *StorageNodeOpsReconciler, ops *simplyblockv1alpha2.StorageNodeOps) bool {
	t.Helper()
	done, err := r.perform(context.Background(), ops, stepHolding)
	if err == nil {
		return !done
	}
	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("holding: %v", err)
	}
	if blocked.reason != MaintenanceQueued {
		t.Errorf("the hold is announced as %q, want %q", blocked.reason, MaintenanceQueued)
	}
	return true
}

// A cluster that says nothing about concurrency takes one worker at a time,
// which is the safe reading of a cluster whose fault tolerance is unknown.
func TestOneWorkerAtATimeIsWhatAnUnstatedConcurrencyMeans(t *testing.T) {
	other, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	r, _ := anOpsWorld(t, aControlPlane(), other, itsNode)

	if !held(t, r, aWindow("a-window")) {
		t.Error("a second worker entered maintenance although the cluster stated no concurrency")
	}
}

// What the cluster says it can afford is what the gate admits.
func TestTheClusterSaysHowManyWorkersMayBeDownAtOnce(t *testing.T) {
	other, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode)
	allowing(t, apiClient, 2)

	if held(t, r, aWindow("a-window")) {
		t.Error("the window waited although the cluster allows two workers at once")
	}
}

// A window still in Holding holds no slot. Counting one would let a deployment
// deadlock with every window waiting for every other.
func TestAWindowStillWaitingHoldsNoSlot(t *testing.T) {
	queued, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	queued.Status.Step = kubeStep(stepHolding)
	r, _ := anOpsWorld(t, aControlPlane(), queued, itsNode)

	if held(t, r, aWindow("a-window")) {
		t.Error("a window queued behind the same gate was counted as occupying it")
	}
}

// A finished window holds nothing either, whatever its last step was.
func TestAFinishedWindowHoldsNoSlot(t *testing.T) {
	over, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	over.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded
	r, _ := anOpsWorld(t, aControlPlane(), over, itsNode)

	if held(t, r, aWindow("a-window")) {
		t.Error("a window that has finished was still counted as occupying a slot")
	}
}

// The count is by worker, so a sibling socket of the same host is not a second
// worker's worth of unavailability. This is the multi-socket case the gate
// exists to get right.
func TestASiblingSocketOfTheSameWorkerIsNotASecondWorker(t *testing.T) {
	sibling, itsNode := aRunningWindow("the-siblings-window", "the-sibling", opsWorker)
	r, _ := anOpsWorld(t, aControlPlane(), sibling, itsNode)

	if held(t, r, aWindow("a-window")) {
		t.Error("the second socket of the worker already in maintenance waited for itself")
	}
}

// Another cluster's maintenance is another cluster's business.
func TestAWindowInAnotherClusterDoesNotHoldThisOneBack(t *testing.T) {
	elsewhere, itsNode := aRunningWindow("another-clusters-window", "another-node", "worker-9")
	itsNode.Spec.ClusterRef = "another-cluster"
	r, _ := anOpsWorld(t, aControlPlane(), elsewhere, itsNode)

	if held(t, r, aWindow("a-window")) {
		t.Error("a window in another cluster held this one back")
	}
}

// The budget exists before the shutdown is issued, which is the ordering the
// whole action rests on.
func TestTheEvictionIsBlockedBeforeTheNodeIsTakenDown(t *testing.T) {
	api := aControlPlane()
	r, apiClient := anOpsWorld(t, api, aReadyStoragePod(opsWorker))

	done, err := r.perform(context.Background(), aWindow("a-window"), stepShuttingDown)
	if err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if done {
		t.Error("the step finished while the node was still online")
	}
	if asked := api.asked("ShutdownNode"); asked != 1 {
		t.Errorf("ShutdownNode was issued %d time(s), want once", asked)
	}

	budget := budgetFor(t, apiClient, opsWorker)
	if budget == nil {
		t.Fatal("no budget holds the eviction, so a drain would evict the pod under a live node")
	}
	if budget.Spec.MaxUnavailable == nil || budget.Spec.MaxUnavailable.IntVal != 0 {
		t.Errorf("maxUnavailable = %v, want 0 while the node is being taken down",
			budget.Spec.MaxUnavailable)
	}
}

// A node that is already offline is where the shutdown was taking it.
func TestAnOfflineNodeNeedsNoShutdown(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	r, _ := anOpsWorld(t, api, aReadyStoragePod(opsWorker))

	done, err := r.perform(context.Background(), aWindow("a-window"), stepShuttingDown)
	if err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if !done {
		t.Error("the step did not finish against a node that is already offline")
	}
	if asked := api.asked("ShutdownNode"); asked != 0 {
		t.Errorf("ShutdownNode was issued %d time(s) against an offline node", asked)
	}
}

// Mid-restart is not a state to shut down from: the call would be refused, and
// the node is on its way somewhere anyway.
func TestANodeMidRestartIsWaitedForRatherThanShutDown(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusInRestart)
	r, _ := anOpsWorld(t, api, aReadyStoragePod(opsWorker))

	done, err := r.perform(context.Background(), aWindow("a-window"), stepShuttingDown)
	if err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	if done {
		t.Error("the step finished against a node whose restart is still running")
	}
	if asked := api.asked("ShutdownNode"); asked != 0 {
		t.Errorf("ShutdownNode was issued %d time(s) into an in-flight restart", asked)
	}
}

// Releasing relaxes the budget the window created and finishes only once the pod
// has actually gone, which is what the drain was waiting to do.
func TestReleasingRelaxesTheBudgetAndWaitsForThePodToGo(t *testing.T) {
	r, apiClient := anOpsWorld(t, aControlPlane(), aReadyStoragePod(opsWorker))
	ops := aWindow("a-window")

	if _, err := r.perform(context.Background(), ops, stepShuttingDown); err != nil {
		t.Fatalf("shutting down: %v", err)
	}

	done, err := r.perform(context.Background(), ops, stepReleasing)
	if err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if done {
		t.Error("the step finished while the storage pod was still on the worker")
	}
	budget := budgetFor(t, apiClient, opsWorker)
	if budget == nil || budget.Spec.MaxUnavailable == nil ||
		budget.Spec.MaxUnavailable.IntVal != 1 {
		t.Errorf("the budget is %v, want the one eviction the drain is waiting on", budget)
	}

	if err := apiClient.Delete(context.Background(), aReadyStoragePod(opsWorker)); err != nil {
		t.Fatalf("evicting the storage pod: %v", err)
	}
	done, err = r.perform(context.Background(), ops, stepReleasing)
	if err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if !done {
		t.Error("the step did not finish although the pod has left the worker")
	}
}

// The node comes back on the host it left, and a restart is not issued against
// one that is already back or already on its way.
func TestTheNodeIsRestartedOnceTheHostIsBack(t *testing.T) {
	api := aControlPlane().reporting(nodeStatusOffline)
	r, _ := anOpsWorld(t, api)

	done, err := r.perform(context.Background(), aWindow("a-window"), stepRestarting)
	if err != nil {
		t.Fatalf("restarting: %v", err)
	}
	if done {
		t.Error("the step finished before the node had come back")
	}
	if asked := api.asked("RestartNode"); asked != 1 {
		t.Errorf("RestartNode was issued %d time(s), want once", asked)
	}
	if api.restarts[0].NodeAddress != "" {
		t.Errorf("the restart names address %q; the node is coming back on the host it left",
			api.restarts[0].NodeAddress)
	}

	restarting, _ := anOpsWorld(t, aControlPlane().reporting(nodeStatusInRestart))
	if done, err := restarting.perform(context.Background(),
		aWindow("a-window"), stepRestarting); err != nil || done {
		t.Errorf("done, err = %v, %v; a restart in flight is waited for", done, err)
	}

	back := aControlPlane()
	online, _ := anOpsWorld(t, back)
	done, err = online.perform(context.Background(), aWindow("a-window"), stepRestarting)
	if err != nil {
		t.Fatalf("restarting: %v", err)
	}
	if !done {
		t.Error("the step did not finish against a node that is online again")
	}
	if asked := back.asked("RestartNode"); asked != 0 {
		t.Errorf("RestartNode was issued %d time(s) against a node already back", asked)
	}
}

// Cleanup takes the budget away, so the worker is drainable by the ordinary
// rules again. A budget left behind is what would make it undrainable forever.
func TestCleanupLeavesTheWorkerDrainableAgain(t *testing.T) {
	r, apiClient := anOpsWorld(t, aControlPlane(), aReadyStoragePod(opsWorker))
	ops := aWindow("a-window")

	if _, err := r.perform(context.Background(), ops, stepShuttingDown); err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	done, err := r.perform(context.Background(), ops, stepCleanup)
	if err != nil {
		t.Fatalf("cleaning up: %v", err)
	}
	if !done {
		t.Error("the step did not finish")
	}

	if budget := budgetFor(t, apiClient, opsWorker); budget != nil {
		t.Errorf("the budget %s outlived the window it belonged to", budget.Name)
	}
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: opsNamespace, Name: "storage-node-" + opsWorker}
	if err := apiClient.Get(context.Background(), key, &pod); err != nil {
		t.Fatalf("reading the storage pod: %v", err)
	}
	if _, labeled := pod.Labels[maintenanceLabel]; labeled {
		t.Error("the pod still carries the window's label, which a later budget would select")
	}
}

// A step that belongs to another action is a hand-edited object or a downgrade,
// and neither resolves by reconciling again.
func TestAStepOfAnotherActionEndsTheWindow(t *testing.T) {
	r, _ := anOpsWorld(t, aControlPlane())

	_, err := r.performMaintenanceStep(context.Background(), aWindow("a-window"), stepPromoting)

	var fatal *terminalStepError
	if !errors.As(err, &fatal) {
		t.Errorf("err = %v, want the terminal kind for a step of another action", err)
	}
}

// budgetFor reads one window's budget, or reports that there is none.
func budgetFor(
	t *testing.T, apiClient client.Client, worker string,
) *policyv1.PodDisruptionBudget {
	t.Helper()
	var budget policyv1.PodDisruptionBudget
	key := client.ObjectKey{
		Namespace: opsNamespace,
		Name:      maintenanceBudgetName(opsCluster, worker),
	}
	err := apiClient.Get(context.Background(), key, &budget)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the maintenance budget: %v", err)
	}
	return &budget
}

// kubeStep is a persisted step, which is what an operation somebody else is
// running carries in its status.
func kubeStep(s step) statemachine.KubeSnapshot {
	return statemachine.KubeSnapshot{State: string(s)}
}
