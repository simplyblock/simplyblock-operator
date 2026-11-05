package node

import (
	"context"
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// Regression: 2026-11-05-a-queued-window-leaves-its-worker-drainable. A master
// MachineConfig rollout on dev-ocp, three storage nodes on the three masters.
// The rollout drained master-3 as soon as master-2 reported Ready, while
// master-2's storage node was still offline. master-3's window waited in Pending
// behind the cluster gate ("degraded rather than active") with no budget up, so
// the drain evicted its SPDK pod anyway: two nodes down, cluster suspended, every
// node restarted, fio failed on every client.

// aWaitingWindow is a window on the fixture's worker that has not been admitted.
func aWaitingWindow(name string) *simplyblockv1alpha2.StorageNodeOps {
	ops := aWindow(name)
	ops.Finalizers = []string{OpsFinalizer}
	return ops
}

// aDegradedCluster is the cluster the previous worker's window leaves behind
// until its node is back.
func aDegradedCluster() *deliveredCluster {
	return &deliveredCluster{synced: true, reading: subscriptions.ClusterDTO{
		ID: opsClusterID, Status: clusterStatusDegraded,
	}}
}

// aWaitingWorld is a window held at the cluster gate, after the pass that raised
// its guard.
func aWaitingWorld(t *testing.T) (*StorageNodeOpsReconciler, client.Client, string) {
	t.Helper()
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), aWaitingWindow("a-window"), spdk)
	r.Clusters = aDegradedCluster()
	pass(t, r, "a-window")
	if budgetFor(t, apiClient) == nil {
		t.Fatal("precondition: the waiting window raised no budget")
	}
	return r, apiClient, spdk.Name
}

// nodeReports sets the lifecycle the control plane reports for the fixture's
// node, which is what tells a waiting window whether there is anything left to
// guard.
func nodeReports(t *testing.T, apiClient client.Client, status string) {
	t.Helper()
	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	node.Status.Status = status
	node.Status.Phase = phaseOf(NodeReading{Status: status})
	if err := apiClient.Status().Update(context.Background(), &node); err != nil {
		t.Fatalf("reporting the node %s: %v", status, err)
	}
}

func selfBudgetUp(t *testing.T, apiClient client.Client) bool {
	t.Helper()
	var budget policyv1.PodDisruptionBudget
	key := client.ObjectKey{Namespace: opsNamespace, Name: selfBudgetName}
	err := apiClient.Get(context.Background(), key, &budget)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("reading the manager's own budget: %v", err)
	}
	return true
}

func TestAWindowTheClusterGateHoldsGuardsItsWorker(t *testing.T) {
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), aWaitingWindow("a-window"), spdk)
	r.Clusters = aDegradedCluster()

	pass(t, r, "a-window")

	if got := operationRead(t, apiClient, "a-window"); got.Status.Phase !=
		simplyblockv1alpha2.StorageNodeOpsPhasePending {
		t.Fatalf("phase = %q, want the window held at admission", got.Status.Phase)
	}
	budget := budgetFor(t, apiClient)
	if budget == nil {
		t.Fatal("no budget holds the eviction while the window waits, " +
			"so the drain that raised it takes the node down out of turn")
	}
	if budget.Spec.MaxUnavailable == nil || budget.Spec.MaxUnavailable.IntVal != 0 {
		t.Errorf("maxUnavailable = %v, want 0 while the window waits", budget.Spec.MaxUnavailable)
	}
	if !guarded(t, apiClient, spdk.Name) {
		t.Error("the SPDK pod carries no label the budget selects, so the budget holds nothing")
	}
}

func TestAWindowWaitingForASlotGuardsItsWorker(t *testing.T) {
	other, itsNode := aRunningWindow("another-window", "another-node", "worker-9")
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), other, itsNode, spdk)

	if !held(t, r, aWindow("a-window")) {
		t.Fatal("the window was admitted although the only slot is taken")
	}
	if budgetFor(t, apiClient) == nil {
		t.Error("no budget holds the eviction while the window waits for a slot")
	}
	if !guarded(t, apiClient, spdk.Name) {
		t.Error("the SPDK pod is not held while the window waits for a slot")
	}
}

// The guard a waiting window raises has to come down with the window. A window
// that never ran used to hold nothing, so deleting one cost nothing; a budget at
// maxUnavailable=0 that nobody removes makes the worker undrainable for good.
func TestADeletedWindowThatNeverRanLeavesTheWorkerDrainable(t *testing.T) {
	r, apiClient, spdk := aWaitingWorld(t)

	if err := apiClient.Delete(context.Background(), operationRead(t, apiClient, "a-window")); err != nil {
		t.Fatalf("deleting the window: %v", err)
	}
	pass(t, r, "a-window")

	if budgetFor(t, apiClient) != nil {
		t.Error("the deleted window's budget is still up, so the worker cannot be drained")
	}
	if guarded(t, apiClient, spdk) {
		t.Error("the deleted window's label is still on the SPDK pod")
	}
}

// A finished window cleared its markers on its way out, and its deletion — which
// the uncordon does, at about the moment the next worker's window enters
// ShuttingDown — must not clear them again: the manager's own budget is one
// object for the namespace, and the next window may be holding it.
func TestDeletingAFinishedWindowLeavesTheNextWindowsSelfBudget(t *testing.T) {
	next := aWindow("next-window")
	next.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseRunning
	next.Status.Step = kubeStep(stepShuttingDown)
	finished, itsNode := aRunningWindow("finished-window", "finished-node", "worker-9")
	finished.Status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded
	finished.Finalizers = []string{OpsFinalizer}
	r, apiClient := anOpsWorld(t, aControlPlane(), next, finished, itsNode)
	r.Workload.ManagerNode = opsWorker
	if err := r.Workload.ProtectSelf(context.Background(), opsNamespace, opsWorker); err != nil {
		t.Fatalf("holding the manager for the next window: %v", err)
	}

	if err := apiClient.Delete(context.Background(), operationRead(t, apiClient, "finished-window")); err != nil {
		t.Fatalf("deleting the finished window: %v", err)
	}
	pass(t, r, "finished-window")

	if !selfBudgetUp(t, apiClient) {
		t.Error("deleting a finished window took down the manager's budget " +
			"that the next worker's window is shutting its node down behind")
	}
}

// The cordon being undone calls a waiting window off, and the guard it raised
// goes with it: an aborted window on a worker nobody is draining must not leave
// the worker undrainable.
func TestAnUncordonCallsOffAWaitingWindowAndItsGuard(t *testing.T) {
	r, apiClient, spdk := aWaitingWorld(t)

	ops := operationRead(t, apiClient, "a-window")
	ops.Spec.Abort = true
	if err := apiClient.Update(context.Background(), ops); err != nil {
		t.Fatalf("calling the window off: %v", err)
	}
	// One pass takes the lock and enters Holding, the next unwinds from it.
	pass(t, r, "a-window")
	pass(t, r, "a-window")

	if got := operationRead(t, apiClient, "a-window"); got.Status.Phase !=
		simplyblockv1alpha2.StorageNodeOpsPhaseAborted {
		t.Fatalf("phase = %q, want the window called off", got.Status.Phase)
	}
	if budgetFor(t, apiClient) != nil {
		t.Error("a window that was called off left a budget behind")
	}
	if guarded(t, apiClient, spdk) {
		t.Error("a window that was called off left its label on the SPDK pod")
	}
}

// A node that is already down is what keeps its cluster unready, so guarding its
// worker would hold the drain until the node is back — and the drain may be the
// repair that brings it back. A guard raised while it was up comes down, and on
// the manager's own worker the manager's budget comes down with it: a window
// held at the gate never reaches Releasing, which is otherwise what releases it.
func TestAWaitingWindowOnANodeAlreadyDownHoldsNoDrain(t *testing.T) {
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), aWaitingWindow("a-window"), spdk)
	r.Workload.ManagerNode = opsWorker
	r.Clusters = aDegradedCluster()
	pass(t, r, "a-window")
	if budgetFor(t, apiClient) == nil || !selfBudgetUp(t, apiClient) {
		t.Fatal("precondition: the waiting window raised no budget while its node was up")
	}

	nodeReports(t, apiClient, nodeStatusOffline)
	pass(t, r, "a-window")

	if budgetFor(t, apiClient) != nil {
		t.Error("the worker of a node that is already down stays undrainable")
	}
	if selfBudgetUp(t, apiClient) {
		t.Error("the manager's own budget stays up for a node that is already down, " +
			"so its converged worker stays undrainable")
	}
}

// A guard standing down on one worker must not take the manager's budget out
// from under a window on the manager's own worker.
func TestAGuardStandingDownElsewhereLeavesTheManagersBudget(t *testing.T) {
	r, apiClient, _ := aWaitingWorld(t)
	r.Workload.ManagerNode = "worker-9"
	if err := r.Workload.ProtectSelf(context.Background(), opsNamespace, "worker-9"); err != nil {
		t.Fatalf("holding the manager for worker-9's window: %v", err)
	}

	nodeReports(t, apiClient, nodeStatusOffline)
	pass(t, r, "a-window")

	if !selfBudgetUp(t, apiClient) {
		t.Error("a window on worker-1 standing down took the manager's budget on worker-9 with it")
	}
}

// A suspended node still runs SPDK, and ShuttingDown sends it a graceful
// shutdown for that reason: the drain must not reach it first. An unreachable
// node is usually a blip, and a guard that came down on one would hand the pod to
// the drain's next retry. Both keep their guard.
func TestAWaitingWindowKeepsItsGuardOnANodeTheDrainWouldStillHurt(t *testing.T) {
	for _, status := range []string{nodeStatusSuspended, utils.NodeStatusUnreachable, ""} {
		t.Run(status, func(t *testing.T) {
			r, apiClient, spdk := aWaitingWorld(t)

			nodeReports(t, apiClient, status)
			pass(t, r, "a-window")

			if budgetFor(t, apiClient) == nil {
				t.Errorf("the guard came down on a %q node, whose SPDK process a drain would kill", status)
			}
			if !guarded(t, apiClient, spdk) {
				t.Errorf("the SPDK pod of a %q node lost its label", status)
			}
		})
	}
}
