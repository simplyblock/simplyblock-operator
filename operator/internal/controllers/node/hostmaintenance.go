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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
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
		return r.maintenanceRelease(ctx, ops, node)
	case stepAwaitingHost:
		return r.maintenanceAwaitHost(ctx, node)
	case stepRestarting:
		return r.maintenanceRestart(ctx, ops, clusterID, nodeID)
	case stepCleanup:
		return r.maintenanceCleanup(ctx, ops, node)
	default:
		return false, fatalf("step %s does not belong to the HostMaintenance action", current)
	}
}

// maintenanceHold is the concurrency gate, and it is a cluster-wide count.
//
// The window guards its worker before it asks the gate anything, for the reason
// guardWaitingWindow gives: a window waiting for its turn with no budget up is a
// queue the drain does not stand in.
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

	if err := r.guardWorker(ctx, node); err != nil {
		return false, err
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

// guardWaitingWindow holds the eviction of a maintenance window's worker while
// the window waits to be admitted.
//
// A window is raised because its worker was cordoned, and the drain that follows
// the cordon does not wait for the window's turn: it evicts the SPDK pod as soon
// as nothing stops it. The window itself waits twice before ShuttingDown raises
// the budget. It waits in Pending while the cluster gate holds it, and the gate
// holds it in exactly the state the previous worker's window leaves behind: a
// cluster degraded until that node is back, then rebalancing. It waits again in
// Holding while the concurrency gate counts the slots.
//
// Nothing guarded the worker during either wait, so the queue held the window and
// not the drain. The drain took the queued worker's node down while the previous
// worker's node was still on its way back, which is two nodes down at once and a
// suspended cluster at fault tolerance one. Guarding the worker from the first
// pass is what makes the drain stand in the queue too.
//
// The guard is not lost on the way in or out. ShuttingDown raises the same budget
// again, idempotently, and Releasing is still what lets the drain through. An
// abort or a failure takes it down through clearMaintenanceMarkers, and so does
// deleting a window that never ran (teardown).
//
// Regression: 2026-11-05-a-queued-window-leaves-its-worker-drainable.
func (r *StorageNodeOpsReconciler) guardWaitingWindow(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) error {
	if ops.Spec.Action != simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance ||
		running(ops) || ops.Spec.Abort {
		return nil
	}
	node, err := r.node(ctx, ops)
	if apierrors.IsNotFound(err) {
		// The lock fails an operation on a node that does not exist.
		return nil
	}
	if err != nil {
		return err
	}
	if node.IsRemote() {
		// A remote node's worker is the peer site's to guard.
		return nil
	}
	if nodeAlreadyDown(node.Status.Status) {
		// A node that is already down has no SPDK process to protect, and its
		// cluster stays unready because of it, so a guard would hold the drain
		// for as long as the node stays down — including the maintenance that
		// is meant to bring it back. A guard raised while it was still up comes
		// down, unless a sibling socket's window on the same worker needs it.
		//
		// Both budgets come down. The manager's own would otherwise stand until
		// the window reached Releasing, which it cannot while the gate holds it
		// on this very node: on a converged worker that is the same drain
		// blocked one object over.
		guarded, err := r.workerStillGuarded(ctx, ops, node)
		if err != nil || guarded {
			return err
		}
		if err := r.Workload.ReleaseSelfOn(ctx, node.Namespace, node.Spec.WorkerNode); err != nil {
			return err
		}
		return r.Workload.AllowEviction(ctx, node.Namespace, node.Spec.ClusterRef, node.Spec.WorkerNode)
	}
	return r.guardWorker(ctx, node)
}

// nodeAlreadyDown reports a node, by the lifecycle the control plane reports
// for it, whose SPDK process is gone or going and which no drain can therefore
// harm: shut down, or leaving the cluster.
//
// It is the control plane's status rather than the phase, because the phase
// folds in nodes a drain would still hurt. A suspended node reads as Offline
// and still runs SPDK — ShuttingDown sends it a graceful shutdown for that
// reason, and so must not have let the drain kill it first. An unreachable or
// timed-out node reads as Failed and is usually a blip; a guard that came down
// on one would hand the pod to the drain's next retry, and going back up a pass
// later is too late. Those keep their guard, and the window waits with them.
//
// An unknown or empty status is a node this cannot vouch for, and it is guarded.
func nodeAlreadyDown(status string) bool {
	return status == nodeStatusOffline || status == nodeStatusInShutdown ||
		utils.NodeIsLeaving(status)
}

// guardWorker raises the budgets that make a drain of the node's worker wait.
//
// The manager's own budget goes up first, for the reason maintenanceShutDown
// gives: on a converged worker the drain may reach the manager before the
// storage pod, and then nothing is left to raise the storage pod's budget.
func (r *StorageNodeOpsReconciler) guardWorker(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) error {
	if err := r.Workload.ProtectSelf(ctx, node.Namespace, node.Spec.WorkerNode); err != nil {
		return err
	}
	err := r.Workload.BlockEviction(ctx, node.Namespace, node.Spec.ClusterRef, node.Spec.WorkerNode)
	if err != nil {
		return fmt.Errorf("hold the eviction of worker %s: %w", node.Spec.WorkerNode, err)
	}
	return nil
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
	// A node leaving the cluster belongs to its removal. The worker is still
	// guarded and released like any other, and the node is sent nothing: a
	// shutdown writes over the removal's status (§10).
	if reading.Status == nodeStatusOffline || utils.NodeIsLeaving(reading.Status) {
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
	_, err = r.once(ctx, ops, func() error {
		if err := r.API.ShutdownNode(ctx, clusterID, nodeID); err != nil {
			return fmt.Errorf("shut down node %s for maintenance: %w", ops.Spec.NodeRef, err)
		}
		return nil
	})
	return false, err
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
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	if err := r.Workload.ReleaseSelf(ctx, node.Namespace); err != nil {
		return false, err
	}

	// The budget is the worker's rather than this node's, so the socket that
	// gets here first does not own it alone.
	guarded, err := r.workerStillGuarded(ctx, ops, node)
	if err != nil {
		return false, err
	}
	if !guarded {
		err := r.Workload.AllowEviction(ctx, node.Namespace,
			node.Spec.ClusterRef, node.Spec.WorkerNode)
		if err != nil {
			return false, fmt.Errorf("release the eviction of worker %s: %w",
				node.Spec.WorkerNode, err)
		}
	}

	// The step still finishes on this node's own pod. Waiting for the sibling's
	// as well would make each socket's progress the other's business, and the
	// host they share is going down for both of them anyway.
	return r.Workload.SpdkPodGone(ctx, node.Namespace, node.Spec.WorkerNode)
}

// workerStillGuarded reports whether another maintenance window on the same
// worker has yet to reach the step that gives the worker's budget up.
//
// A multi-socket worker runs one window per socket, and the concurrency gate
// admits them together on purpose: the pair is one worker's worth of
// unavailability (§10). The budget and the label they hold the eviction with
// are the worker's, so they share them, and the socket whose backend node goes
// offline first must not drop a guard the other's SPDK process is still
// standing behind — the drain would evict a live one, which is the eviction
// this whole action exists to prevent.
//
// A sibling that has reached Releasing has already asked for the budget to go,
// so only one before it counts. A sibling with no step recorded counts too: it
// has not started, so its ShuttingDown is still ahead of it.
func (r *StorageNodeOpsReconciler) workerStillGuarded(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	var operations simplyblockv1alpha2.StorageNodeOpsList
	if err := r.List(ctx, &operations, client.InNamespace(ops.Namespace)); err != nil {
		return false, fmt.Errorf("list the namespace's node operations: %w", err)
	}

	for i := range operations.Items {
		other := &operations.Items[i]
		if other.Name == ops.Name ||
			other.Spec.Action != simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance ||
			terminalOps(other.Status.Phase) ||
			!guardingStep(step(other.Status.Step.State)) {
			continue
		}
		var target simplyblockv1alpha2.StorageNode
		key := types.NamespacedName{Name: other.Spec.NodeRef, Namespace: other.Namespace}
		if err := r.Get(ctx, key, &target); err != nil {
			// A window whose node cannot be read is one whose worker cannot be
			// established, and the safe reading of that is that it might be this
			// one's. Leaving the guard up costs a drain that waits; taking it
			// down on a guess costs a live SPDK process.
			return true, nil
		}
		if target.Spec.ClusterRef != node.Spec.ClusterRef ||
			target.Spec.WorkerNode != node.Spec.WorkerNode {
			continue
		}
		return true, nil
	}
	return false, nil
}

// guardingStep reports the steps a window still needs the worker's budget in.
func guardingStep(current step) bool {
	switch current {
	case "", stepHolding, stepShuttingDown:
		return true
	default:
		return false
	}
}

// callableOff reports the steps a window can still be called off from when the
// cordon that raised it is undone.
//
// An unstarted window and one holding for a slot have both done nothing to the
// node. From ShuttingDown onward it is down and something has to bring it back,
// so the window runs on and the uncordon it is waiting for is the one
// AwaitingHost reads.
func callableOff(current step) bool {
	return current == "" || current == stepHolding
}

// maintenanceAwaitHost is the step whose length nobody controls. An OS upgrade and
// a reboot take as long as they take, and the node's lock is held throughout. That
// is correct, and it is why this step's deadline is generous enough for a firmware
// update: an expiry fails the operation and leaves the node offline, needing a
// Restart to recover, so the deadline is a detection mechanism rather than a
// recovery one.
//
// It ends when the worker is schedulable again and its API answers. The API alone
// is not enough: a cordoned, never-rebooted worker keeps answering.
//
// Regression: 2026-10-13-maintenance-does-not-wait-for-the-uncordon.
func (r *StorageNodeOpsReconciler) maintenanceAwaitHost(
	ctx context.Context, node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	var worker corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: node.Spec.WorkerNode}, &worker); err != nil {
		// A missing worker keeps the step waiting; the step deadline reports it.
		return false, client.IgnoreNotFound(err)
	}
	if worker.Spec.Unschedulable {
		return false, nil
	}
	return r.Workload.HostAnswers(ctx, node.Namespace, node.Spec.ClusterRef, node.Spec.WorkerNode)
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
	// A node leaving the cluster is not brought back: a restart returns it to
	// service in the middle of its removal, or after the control plane gave up
	// on one, which is the removal's to drive again rather than the window's.
	if reading.Status == nodeStatusOnline || utils.NodeIsLeaving(reading.Status) {
		return true, nil
	}
	if reading.Status == nodeStatusInRestart {
		return false, nil
	}
	params := RestartParams{
		Force:          boolValue(ops.Spec.Force),
		ReattachVolume: boolValue(ops.Spec.ReattachVolume),
	}
	_, err = r.once(ctx, ops, func() error {
		if err := r.API.RestartNode(ctx, clusterID, nodeID, params); err != nil {
			return fmt.Errorf("restart node %s after maintenance: %w", ops.Spec.NodeRef, err)
		}
		return nil
	})
	return false, err
}

// maintenanceCleanup removes what the window put in place, so the worker is
// drainable by the ordinary rules again.
//
// The manager's own budget is released in Releasing and cleared again here,
// which is not a repetition: Releasing is reached only on the way through, and
// this step runs on the way out.
func (r *StorageNodeOpsReconciler) maintenanceCleanup(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
) (bool, error) {
	if err := r.clearMaintenance(ctx, ops, node); err != nil {
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
// The two removals are independent, so both are attempted and their errors are
// reported together. Stopping at the manager's budget would leave the worker's
// standing, and the worker's is the one that makes a host undrainable.
func (r *StorageNodeOpsReconciler) clearMaintenance(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	node *simplyblockv1alpha2.StorageNode,
) error {
	failures := []error{r.Workload.ReleaseSelf(ctx, node.Namespace)}

	// The worker's markers are shared with a sibling socket's window, and a
	// window ending is not a reason to take a guard out from under one that is
	// still running. The last of them to end is what clears them.
	guarded, err := r.workerStillGuarded(ctx, ops, node)
	switch {
	case err != nil:
		failures = append(failures, err)
	case !guarded:
		failures = append(failures, r.Workload.ClearEvictionBudget(ctx, node.Namespace,
			node.Spec.ClusterRef, node.Spec.WorkerNode))
	}
	return errors.Join(failures...)
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
	if err != nil {
		// A node that cannot be read is the case where the markers are certain to
		// be left and the worker they are on cannot even be named, so it is the
		// case that most needs saying out loud. Staying silent here was the one
		// path on which the event this teardown owes never arrived.
		r.emit(ctx, ops, corev1.EventTypeWarning, MaintenanceMarkersLeft, fmt.Sprintf(
			"This window's markers were left in place because node %s could not be "+
				"read to find the worker they are on: %v", ops.Spec.NodeRef, err))
		return
	}
	if err := r.clearMaintenance(ctx, ops, node); err != nil {
		// Which marker survived is not known here: the budget may be gone and the
		// pod labels left, or the reverse. The event says what to look for rather
		// than asserting which one it is.
		r.emit(ctx, ops, corev1.EventTypeWarning, MaintenanceMarkersLeft, fmt.Sprintf(
			"This window's eviction budget or pod labels may remain on worker %s, "+
				"which is not drainable by the ordinary rules until they are "+
				"removed by hand: %v", node.Spec.WorkerNode, err))
	}
}
