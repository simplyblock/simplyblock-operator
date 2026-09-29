// The HostMaintenance action: surviving a Kubernetes node drain.
//
// A Kubernetes worker being drained for an OS upgrade takes its storage-node pod
// with it. Left alone, the SPDK process is killed underneath a running backend
// node, which the control plane sees as a node that vanished. This action is how
// the node is taken down deliberately, allowed out of the way, and brought back.
//
//	Holding ──► ShuttingDown ──► Releasing ──► AwaitingHost ──► Restarting ──► Cleanup
//
// The operator raises it, and a user does not. The trigger is the worker being
// cordoned, which the StorageNode controller sees by watching Kubernetes Node
// objects. A user creating one by hand is accepted and behaves identically, which
// is what makes the flow testable without cordoning anything.
//
// Modeling it as a StorageNodeOps rather than as its own controller is what gives
// it the discipline the other actions have: it takes the node's lock, so nothing
// else touches a node whose host is rebooting, its position is a persisted step
// rather than a phase string in a fleet object's status, and it is an audit record
// of a maintenance window afterward. That retires the eight-phase drain
// coordinator the retired StorageNodeSet carried (§15.3).
//
// The PodDisruptionBudget runs backward from the usual one. A per-node budget with
// no disruption allowed is created *before* the shutdown, so `kubectl drain` blocks
// on it while the backend node is being taken down gracefully. Relaxing it in
// Releasing is what lets the drain proceed. The budget's job is therefore to hold
// the eviction until the storage node is safely offline, rather than to keep a
// replica count up.
//
// design-storagenode.md §10 is the specification.

package node

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// performMaintenanceStep runs one step of a maintenance window.
func (r *StorageNodeOpsReconciler) performMaintenanceStep(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, current step,
) (bool, error) {
	node, err := r.node(ctx, ops)
	if err != nil {
		return false, err
	}

	// Holding performs nothing and asks only about its peers, so it is the one
	// step that does not need the node to exist in the control plane yet.
	if current == stepHolding {
		return r.maintenanceHold(ctx, ops, node)
	}

	clusterID, nodeID, err := r.target(ctx, ops)
	if err != nil {
		return false, err
	}

	switch current {
	case stepShuttingDown:
		return r.maintenanceShutDown(ctx, ops, node, clusterID, nodeID)
	case stepReleasing:
		return r.maintenanceRelease(ctx, node)
	case stepAwaitingHost:
		return r.maintenanceAwaitHost(ctx, node)
	case stepRestarting:
		return r.maintenanceRestart(ctx, ops, clusterID, nodeID)
	case stepCleanup:
		return r.maintenanceCleanup(ctx, node)
	default:
		return false, fatalf("step %s does not belong to the HostMaintenance action", current)
	}
}

// maintenanceHold is the concurrency gate, and it is a cluster-wide count.
//
// How many workers may be in maintenance at once is
// StorageCluster.status.maxConcurrentWorkerRestarts, which is the smaller of what
// the cluster asks for and the fault tolerance the control plane reports. Counting
// operations rather than nodes is what makes the gate correct across a
// multi-socket worker: two nodes on one host go into maintenance together, and the
// pair is one worker's worth of unavailability — so the count is by distinct
// worker.
func (r *StorageNodeOpsReconciler) maintenanceHold(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Name: node.Spec.ClusterRef, Namespace: node.Namespace}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return false, fmt.Errorf("read cluster %s: %w", node.Spec.ClusterRef, err)
	}

	limit := int32(1)
	if effective := cluster.Status.MaxConcurrentWorkerRestarts; effective != nil && *effective > 0 {
		limit = *effective
	}

	inFlight, err := r.workersInMaintenance(ctx, ops, node)
	if err != nil {
		return false, err
	}
	if int32(len(inFlight)) >= limit {
		return false, blockedf(MaintenanceQueued,
			"%d of %d maintenance slots are taken by %v; this window waits its turn",
			len(inFlight), limit, inFlight)
	}
	return true, nil
}

// workersInMaintenance names the distinct workers whose maintenance window has
// passed Holding, excluding this operation's own.
//
// A window still in Holding holds no slot: it is queued behind the same gate, and
// counting it would let a deployment deadlock with every window waiting for every
// other.
func (r *StorageNodeOpsReconciler) workersInMaintenance(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
) ([]string, error) {
	var operations simplyblockv1alpha2.StorageNodeOpsList
	if err := r.List(ctx, &operations, client.InNamespace(ops.Namespace)); err != nil {
		return nil, fmt.Errorf("list the namespace's node operations: %w", err)
	}

	workers := map[string]struct{}{}
	for i := range operations.Items {
		other := &operations.Items[i]
		if other.Name == ops.Name ||
			other.Spec.Action != simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance ||
			terminalOps(other.Status.Phase) ||
			other.Status.Step.State == "" ||
			other.Status.Step.State == string(stepHolding) {
			continue
		}
		var target simplyblockv1alpha2.StorageNode
		key := types.NamespacedName{Name: other.Spec.NodeRef, Namespace: other.Namespace}
		if err := r.Get(ctx, key, &target); err != nil {
			continue
		}
		if target.Spec.ClusterRef != node.Spec.ClusterRef {
			continue
		}
		// The worker this operation is about to take down is already counted when
		// its sibling socket's window is running, which is the multi-socket case
		// the gate exists to get right.
		if target.Spec.WorkerNode == node.Spec.WorkerNode {
			continue
		}
		workers[target.Spec.WorkerNode] = struct{}{}
	}

	names := make([]string, 0, len(workers))
	for worker := range workers {
		names = append(names, worker)
	}
	return names, nil
}

// maintenanceShutDown blocks the eviction and takes the backend node down.
//
// The budget is created before the shutdown is issued, and that ordering is the
// whole mechanism: a drain that reaches the pod before the budget exists evicts it
// under a running SPDK process.
//
// The manager's own budget comes before even that, and for the same reason one
// step further back. On a converged worker the manager runs on the host being
// drained, the drain evicts in no particular order, and an eviction that
// arrives here first takes out the only thing that would ever have created the
// storage node's budget (selfbudget.go).
func (r *StorageNodeOpsReconciler) maintenanceShutDown(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
	clusterID, nodeID string,
) (bool, error) {
	if err := r.Workload.ProtectSelf(ctx, node.Namespace, node.Spec.WorkerNode); err != nil {
		return false, err
	}

	err := r.Workload.BlockEviction(ctx, node.Namespace, node.Spec.ClusterRef, node.Spec.WorkerNode)
	if err != nil {
		return false, fmt.Errorf("hold the eviction of worker %s: %w", node.Spec.WorkerNode, err)
	}

	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if reading.Status == nodeStatusOffline {
		return true, nil
	}
	if reading.Status == nodeStatusInRestart || reading.Status == nodeStatusInShutdown {
		// Neither is a state to shut down from: the call would be refused, and
		// the node is on its way somewhere anyway. Waiting is what lets the next
		// pass see where it landed.
		//
		// in_shutdown is the shutdown this step itself asked for, so re-posting
		// it is not a harmless retry: the control plane answers 409, the step
		// takes that for a failure, and the window spends its budget backing off
		// against its own progress.
		//
		// Regression: 2026-09-29-maintenance-reissues-the-shutdown.
		return false, nil
	}
	if err := r.API.ShutdownNode(ctx, clusterID, nodeID); err != nil {
		return false, fmt.Errorf("shut down node %s for maintenance: %w", ops.Spec.NodeRef, err)
	}
	return false, nil
}

// maintenanceRelease takes the budget away so the eviction the drain is waiting
// on can proceed, and completes when the SPDK pod has actually gone.
//
// The manager stops holding itself here too, and it has to be here rather than
// at the end: on a converged worker the manager is on the node being drained,
// so a budget held past this point would block the drain on the manager instead
// of on the storage pod, which is the same deadlock one object over. What the
// window still needs from the manager after this — waiting for the host and
// restarting the node — survives the manager being rescheduled elsewhere,
// because the step is persisted.
func (r *StorageNodeOpsReconciler) maintenanceRelease(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	if err := r.Workload.ReleaseSelf(ctx, node.Namespace); err != nil {
		return false, err
	}

	err := r.Workload.AllowEviction(ctx, node.Namespace, node.Spec.ClusterRef, node.Spec.WorkerNode)
	if err != nil {
		return false, fmt.Errorf("release the eviction of worker %s: %w", node.Spec.WorkerNode, err)
	}
	return r.Workload.SpdkPodGone(ctx, node.Namespace, node.Spec.WorkerNode)
}

// maintenanceAwaitHost is the step whose length nobody controls. An OS upgrade and
// a reboot take as long as they take, and the node's lock is held throughout. That
// is correct, and it is why this step's deadline is generous enough for a firmware
// update: an expiry fails the operation and leaves the node offline, needing a
// Restart to recover, so the deadline is a detection mechanism rather than a
// recovery one.
func (r *StorageNodeOpsReconciler) maintenanceAwaitHost(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	return r.Workload.HostAnswers(ctx, node.Namespace, node.Spec.WorkerNode)
}

// maintenanceRestart brings the backend node back on the host it left. The call is
// skipped when the node is already restarting or online, which is what makes
// re-entering the step harmless.
func (r *StorageNodeOpsReconciler) maintenanceRestart(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	clusterID, nodeID string,
) (bool, error) {
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}
	if reading.Status == nodeStatusOnline {
		return true, nil
	}
	if reading.Status == nodeStatusInRestart {
		return false, nil
	}
	params := RestartParams{
		Force:          boolValue(ops.Spec.Force),
		ReattachVolume: boolValue(ops.Spec.ReattachVolume),
	}
	if err := r.API.RestartNode(ctx, clusterID, nodeID, params); err != nil {
		return false, fmt.Errorf("restart node %s after maintenance: %w", ops.Spec.NodeRef, err)
	}
	return false, nil
}

// maintenanceCleanup removes what the window put in place, so the worker is
// drainable by the ordinary rules again.
//
// The manager's own budget is released in Releasing and cleared again here,
// which is not a repetition: Releasing is reached only on the way through, and
// this step runs on the way out.
func (r *StorageNodeOpsReconciler) maintenanceCleanup(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	if err := r.clearMaintenance(ctx, node); err != nil {
		return false, err
	}
	return true, nil
}

// clearMaintenance takes down both budgets and the label that selects one of
// them.
//
// It is the whole of what a window leaves on a cluster, which is why the two
// terminal outcomes that do not pass through Cleanup call it too. A budget at
// maxUnavailable=0 outliving the window that raised it makes the worker
// undrainable by anything, forever, with nothing left saying why — and that is
// what a maintenance expiring on its deadline used to produce, because the
// graph is a chain with no edge from a failing step to Cleanup and unwinds()
// names no step of this action.
//
// Regression: 2026-09-29-failed-maintenance-leaves-its-markers.
func (r *StorageNodeOpsReconciler) clearMaintenance(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) error {
	if err := r.Workload.ReleaseSelf(ctx, node.Namespace); err != nil {
		return err
	}
	return r.Workload.ClearEvictionBudget(ctx, node.Namespace,
		node.Spec.ClusterRef, node.Spec.WorkerNode)
}

// clearMaintenanceMarkers is the terminal teardown as the two outcomes that do
// not reach Cleanup reach it: best-effort, and announced rather than retried.
//
// Best-effort for the reason resumeNode is. An operation that cannot reach a
// terminal phase never releases the node's lock, and a budget nobody could
// delete is a smaller problem than a node nothing can ever operate on again.
// The event is what says the worker needs a hand.
func (r *StorageNodeOpsReconciler) clearMaintenanceMarkers(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) {
	if ops.Spec.Action != simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance {
		return
	}
	node, err := r.node(ctx, ops)
	if err != nil || node == nil {
		return
	}
	if err := r.clearMaintenance(ctx, node); err != nil {
		r.emit(ctx, ops, corev1.EventTypeWarning, MaintenanceMarkersLeft, fmt.Sprintf(
			"Worker %s keeps the window's eviction budget and cannot be drained "+
				"until it is deleted by hand: %v", node.Spec.WorkerNode, err))
	}
}
