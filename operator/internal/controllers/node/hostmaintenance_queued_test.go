package node

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
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
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), aWaitingWindow("a-window"), spdk)
	r.Clusters = aDegradedCluster()
	pass(t, r, "a-window")
	if budgetFor(t, apiClient) == nil {
		t.Fatal("precondition: the waiting window raised no budget")
	}

	if err := apiClient.Delete(context.Background(), operationRead(t, apiClient, "a-window")); err != nil {
		t.Fatalf("deleting the window: %v", err)
	}
	pass(t, r, "a-window")

	if budgetFor(t, apiClient) != nil {
		t.Error("the deleted window's budget is still up, so the worker cannot be drained")
	}
	if guarded(t, apiClient, spdk.Name) {
		t.Error("the deleted window's label is still on the SPDK pod")
	}
}

// A window being called off is not about to take anything down, so it raises
// nothing on its way out.
func TestAnAbortedWaitingWindowLeavesNoGuard(t *testing.T) {
	ops := aWaitingWindow("a-window")
	ops.Spec.Abort = true
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), ops, spdk)
	r.Clusters = aDegradedCluster()

	pass(t, r, "a-window")
	pass(t, r, "a-window")

	if budgetFor(t, apiClient) != nil {
		t.Error("a window that was called off left a budget behind")
	}
	if guarded(t, apiClient, spdk.Name) {
		t.Error("a window that was called off left its label on the SPDK pod")
	}
}

// A node that is already down is what keeps its cluster unready, so guarding its
// worker would hold the drain until the node is back — and the drain may be the
// repair that brings it back. A guard raised while it was up comes down.
func TestAWaitingWindowOnANodeAlreadyDownHoldsNoDrain(t *testing.T) {
	spdk := anSpdkPod(opsWorker, "4420")
	r, apiClient := anOpsWorld(t, aControlPlane(), aWaitingWindow("a-window"), spdk)
	r.Clusters = aDegradedCluster()
	pass(t, r, "a-window")
	if budgetFor(t, apiClient) == nil {
		t.Fatal("precondition: the waiting window raised no budget while its node was up")
	}

	var node simplyblockv1alpha2.StorageNode
	key := client.ObjectKey{Namespace: opsNamespace, Name: opsNodeName}
	if err := apiClient.Get(context.Background(), key, &node); err != nil {
		t.Fatalf("reading the node: %v", err)
	}
	node.Status.Phase = simplyblockv1alpha2.StorageNodePhaseOffline
	if err := apiClient.Status().Update(context.Background(), &node); err != nil {
		t.Fatalf("taking the node offline: %v", err)
	}
	pass(t, r, "a-window")

	if budgetFor(t, apiClient) != nil {
		t.Error("the worker of a node that is already down stays undrainable")
	}
}
