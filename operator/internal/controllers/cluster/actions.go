// What each step of each action does, and how it knows it is finished.
//
// Every entry here obeys the same two rules, and both come from §6.2:
//
//   - A step completes on a state, not on a transition. The "finished" half of
//     each step is a predicate over what the control plane reports now, never
//     an observation of a change, because a coalescing stream delivers current
//     truth rather than an edit log and a cluster that moved through three
//     states between two readings arrives as the last one once.
//   - A call is skipped when its target is already at or past the state that
//     call would produce. That is what makes a step recorded without its side
//     effect having fired safe to re-enter: the re-entry reads the state and
//     either acts or advances, so the question a `triggered` flag answers is
//     one this controller never asks.
//
// design-storagecluster.md §6.3 and §6.4 are the specification.

package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// perform runs one step of one action and reports whether it has finished.
//
// A step that has not finished is waiting on the control plane, and the caller
// requeues. An ordinary error is retried; a terminalStepError is not.
func (r *StorageClusterOpsReconciler) perform(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, current step,
) (bool, error) {
	clusterID, err := r.clusterUUIDFor(ctx, ops)
	if err != nil {
		return false, err
	}

	switch current {
	case stepRequesting:
		return r.request(ctx, ops, clusterID)
	case stepAwaiting:
		return r.await(ctx, ops, clusterID)
	case stepShuttingDown:
		return r.shutDownCluster(ctx, clusterID)
	case stepStarting:
		return r.startCluster(ctx, clusterID)
	case stepCheckingPeers, stepShuttingDownNode, stepRefreshingPod,
		stepAwaitingPod, stepRestartingNode, stepRebalancing:
		return r.performNodeStep(ctx, ops, clusterID, current)
	default:
		return false, fatalf("step %s belongs to no action this operator runs", current)
	}
}

// request issues the one call the five single-call actions make. Each is
// skipped when the cluster is already where the call would put it, which is
// what makes re-entering the step after a crash harmless.
func (r *StorageClusterOpsReconciler) request(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, clusterID string,
) (bool, error) {
	log := logf.FromContext(ctx)

	switch ops.Spec.Action {
	case simplyblockv1alpha2.StorageClusterOpsActionActivate:
		if ready, err := r.failureDomainsReady(ctx, ops); err != nil || !ready {
			return false, err
		}
		active, err := r.clusterActive(ctx, clusterID)
		if err != nil {
			return false, err
		}
		if active {
			return true, nil
		}
		if err := r.API.Activate(ctx, clusterID); err != nil {
			return false, fmt.Errorf("activate cluster %s: %w", ops.Spec.ClusterRef, err)
		}

	case simplyblockv1alpha2.StorageClusterOpsActionExpand:
		// An expansion has no state of its own to skip on: a cluster is active
		// before it and active after it, so the only guard is the step record
		// (§13, Q1 leaves what an expansion takes as a parameter open).
		if err := r.API.Expand(ctx, clusterID); err != nil {
			return false, fmt.Errorf("expand cluster %s: %w", ops.Spec.ClusterRef, err)
		}

	case simplyblockv1alpha2.StorageClusterOpsActionShutdown:
		active, err := r.clusterActive(ctx, clusterID)
		if err != nil {
			return false, err
		}
		if !active {
			return true, nil
		}
		if err := r.API.Shutdown(ctx, clusterID); err != nil {
			return false, fmt.Errorf("shut down cluster %s: %w", ops.Spec.ClusterRef, err)
		}

	case simplyblockv1alpha2.StorageClusterOpsActionStart:
		active, err := r.clusterActive(ctx, clusterID)
		if err != nil {
			return false, err
		}
		if active {
			return true, nil
		}
		if err := r.API.Start(ctx, clusterID); err != nil {
			return false, fmt.Errorf("start cluster %s: %w", ops.Spec.ClusterRef, err)
		}

	case simplyblockv1alpha2.StorageClusterOpsActionCancelTask:
		return r.requestCancel(ctx, ops, clusterID)

	default:
		return false, fatalf("action %s does not issue a request", ops.Spec.Action)
	}

	log.Info("the action was accepted by the control plane",
		"operation", ops.Name, "action", ops.Spec.Action, "cluster", ops.Spec.ClusterRef)
	return true, nil
}

// requestCancel asks the control plane to stop one task. A task already gone is
// the outcome the operation asked for reached by another route, so it is
// success rather than an error, and the distinction between "canceled" and
// "finished first" lives in the events rather than in the phase.
func (r *StorageClusterOpsReconciler) requestCancel(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, clusterID string,
) (bool, error) {
	if ops.Spec.CancelTask == nil || ops.Spec.CancelTask.TaskID == "" {
		return false, fatalf("action CancelTask names no task in spec.cancelTask.taskID")
	}
	taskID := ops.Spec.CancelTask.TaskID

	running, err := r.runningTask(ctx, clusterID, taskID)
	if err != nil {
		return false, err
	}
	if !running {
		return true, nil
	}
	if err := r.API.CancelTask(ctx, clusterID, taskID); err != nil {
		return false, fmt.Errorf("cancel task %s: %w", taskID, err)
	}
	return true, nil
}

// await is the second half of the five single-call actions: the completion
// condition each of them waits on.
func (r *StorageClusterOpsReconciler) await(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, clusterID string,
) (bool, error) {
	switch ops.Spec.Action {
	case simplyblockv1alpha2.StorageClusterOpsActionShutdown:
		active, err := r.clusterActive(ctx, clusterID)
		return !active, err

	case simplyblockv1alpha2.StorageClusterOpsActionCancelTask:
		// A cancel the control plane accepted is one it has started, and it
		// finishes the stopping in its own time. What is waited on is the task
		// leaving the running and pending set.
		running, err := r.runningTask(ctx, clusterID, ops.Spec.CancelTask.TaskID)
		if err != nil {
			return false, err
		}
		if !running {
			r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
				TaskCanceled, TaskCanceled,
				"Task %s is no longer running on cluster %s",
				ops.Spec.CancelTask.TaskID, ops.Spec.ClusterRef)
		}
		return !running, nil

	default:
		return r.clusterActive(ctx, clusterID)
	}
}

// runningTask reports whether one task is still in the control plane's running
// or pending set, from the task stream's cache once it has delivered the
// cluster's snapshot and from the control plane until then.
//
// The gate matters more here than anywhere else in this package, because a
// miss is the completion condition: an unsynced cache holds no tasks at all,
// and reading that as "the task is gone" would report every cancel complete
// the moment it was asked for.
func (r *StorageClusterOpsReconciler) runningTask(
	ctx context.Context, clusterID, taskID string,
) (bool, error) {
	scope := cpinformer.Scope{clusterID}
	if r.Tasks != nil && r.Tasks.Synced(scope) {
		task, ok := r.Tasks.Lookup(taskID)
		return ok && !task.Finished(), nil
	}

	tasks, err := r.API.Tasks(ctx, clusterID)
	if err != nil {
		return false, fmt.Errorf("read the tasks of cluster %s: %w", clusterID, err)
	}
	for _, task := range tasks {
		if task.ID == taskID {
			return !task.Finished(), nil
		}
	}
	return false, nil
}

// shutDownCluster is the first half of Restart, and startCluster the second.
// The control plane offers no restart of its own, which is what makes this the
// one action with two side effects (§6.4).
func (r *StorageClusterOpsReconciler) shutDownCluster(
	ctx context.Context, clusterID string,
) (bool, error) {
	active, err := r.clusterActive(ctx, clusterID)
	if err != nil {
		return false, err
	}
	if !active {
		return true, nil
	}
	if err := r.API.Shutdown(ctx, clusterID); err != nil {
		return false, fmt.Errorf("shut down cluster %s: %w", clusterID, err)
	}
	return false, nil
}

func (r *StorageClusterOpsReconciler) startCluster(
	ctx context.Context, clusterID string,
) (bool, error) {
	active, err := r.clusterActive(ctx, clusterID)
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	if err := r.API.Start(ctx, clusterID); err != nil {
		return false, fmt.Errorf("start cluster %s: %w", clusterID, err)
	}
	return false, nil
}

// failureDomainsReady gates an activation on the cluster's failure domains
// holding an equal number of hosts, and reports the hold rather than failing.
//
// It is checked before the activate call ever fires and leaves the operation's
// status where it is on a refusal, so a cluster whose nodes are still arriving
// waits rather than failing. It is a no-op for a cluster that is not in
// failure-domain mode.
func (r *StorageClusterOpsReconciler) failureDomainsReady(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) (bool, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Name: ops.Spec.ClusterRef, Namespace: ops.Namespace}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return false, err
	}
	if !ptr.BoolFromOrFalse(cluster.Spec.EnableFailureDomains) {
		return true, nil
	}

	hosts, err := FailureDomainHosts(ctx, r.Client, cluster.Namespace, cluster.Name)
	if err != nil {
		return false, fmt.Errorf("read the cluster's failure domains: %w", err)
	}
	reason := ActivationDomainCountViolation(StripeParityChunks(cluster.Spec.Stripe), hosts)
	if reason == "" {
		return true, nil
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
		FailureDomainNotReady, FailureDomainNotReady,
		"The activation is waiting on failure-domain readiness: %s", reason)
	return false, nil
}

// StripeParityChunks is the cluster's npcs, which is how many hosts a failure
// domain has to be able to lose. An unstated stripe is read as one parity
// chunk, matching what the control plane defaults to.
//
// It is exported for the same reason the failure-domain rules beside it are:
// the storage-node set's own activation gate asks the same question of the
// same cluster.
func StripeParityChunks(s *simplyblockv1alpha2.StripeSpec) int {
	if s == nil {
		return 1
	}
	return ptr.IntFrom(s.ParityChunks, 1)
}

// stripeDataChunks is the cluster's ndcs.
func stripeDataChunks(s *simplyblockv1alpha2.StripeSpec) int {
	if s == nil {
		return 1
	}
	return ptr.IntFrom(s.DataChunks, 1)
}

// nodeStatus is one node's control-plane status, lowercased, and whether the
// control plane still lists the node at all.
func nodeStatus(nodes []utils.NodeStatusResponse, nodeID string) (string, bool) {
	for _, node := range nodes {
		if node.UUID == nodeID {
			return lower(node.Status), true
		}
	}
	return "", false
}
