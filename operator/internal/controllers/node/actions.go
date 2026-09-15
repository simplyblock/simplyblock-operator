// What each step of each action does, and how it knows it is finished.
//
// Every entry here obeys the same two rules, and both come from §7.2:
//
//   - A step completes on a state, not on a transition. The "finished" half of
//     each step is a predicate over what the control plane reports now, never an
//     observation of a change, because a coalescing stream delivers current truth
//     rather than an edit log and a node that moved through three states between
//     two readings arrives as the last one once.
//   - A call is skipped when its target is already at or past the state that call
//     would produce. That is what makes a step recorded without its side effect
//     having fired safe to re-enter: the re-entry reads the state and either acts
//     or advances, so the question a `triggered` flag answers is one this
//     controller never asks.
//
// The four single-step actions live here in full. The three multi-step ones are
// each a file of their own, because a drain, a relocation, and a maintenance
// window are each a workflow rather than a call.
//
// design-storagenode.md §7.3 is the specification for what is here.

package node

import (
	"context"
	"fmt"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// perform runs one step of one action and reports whether it has finished.
//
// A step that has not finished is waiting on the control plane, and the caller
// requeues. An ordinary error is retried; a terminalStepError is not, and a
// blockedStepError holds with an event.
func (r *StorageNodeOpsReconciler) perform(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, current step,
) (bool, error) {
	switch current {
	case stepRequesting:
		return r.request(ctx, ops)
	case stepAwaiting:
		return r.await(ctx, ops)

	case stepValidating, stepSuspending, stepMigratingVolumes, stepVerifying, stepRemoving:
		return r.performRemoveStep(ctx, ops, current)

	case stepPreparing, stepRelocating, stepAwaitingNode, stepPromoting:
		return r.performMigrateStep(ctx, ops, current)

	case stepHolding, stepShuttingDown, stepReleasing, stepAwaitingHost,
		stepRestarting, stepCleanup:
		return r.performMaintenanceStep(ctx, ops, current)

	default:
		return false, fatalf("step %s belongs to no action this operator runs", current)
	}
}

// request issues the one call the four single-step actions make. Each is skipped
// when the node is already where the call would put it, which is what makes
// re-entering the step after a crash harmless.
func (r *StorageNodeOpsReconciler) request(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (bool, error) {
	clusterID, nodeID, err := r.target(ctx, ops)
	if err != nil {
		return false, err
	}
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}

	switch ops.Spec.Action {
	case simplyblockv1alpha2.StorageNodeOpsActionShutdown:
		if reading.Status == nodeStatusOffline {
			return true, nil
		}
		if err := r.API.ShutdownNode(ctx, clusterID, nodeID); err != nil {
			return false, fmt.Errorf("shut down node %s: %w", ops.Spec.NodeRef, err)
		}

	case simplyblockv1alpha2.StorageNodeOpsActionRestart:
		// A restart has no state of its own to skip on: a node is online before
		// it and online after it. What guards the second call is the step record
		// and the completion condition below, which does not report finished
		// until the node is back.
		params := RestartParams{
			Force:          boolValue(ops.Spec.Force),
			ReattachVolume: boolValue(ops.Spec.ReattachVolume),
		}
		if err := r.API.RestartNode(ctx, clusterID, nodeID, params); err != nil {
			return false, fmt.Errorf("restart node %s: %w", ops.Spec.NodeRef, err)
		}

	case simplyblockv1alpha2.StorageNodeOpsActionSuspend:
		if reading.Status == nodeStatusSuspended {
			return true, nil
		}
		if err := r.API.Suspend(ctx, clusterID, nodeID); err != nil {
			return false, fmt.Errorf("suspend node %s: %w", ops.Spec.NodeRef, err)
		}

	case simplyblockv1alpha2.StorageNodeOpsActionResume:
		if reading.Status == nodeStatusOnline {
			return true, nil
		}
		if err := r.API.Resume(ctx, clusterID, nodeID); err != nil {
			return false, fmt.Errorf("resume node %s: %w", ops.Spec.NodeRef, err)
		}

	default:
		return false, fatalf("action %s does not issue a single request", ops.Spec.Action)
	}
	return true, nil
}

// await is the completion condition of the four single-step actions, read against
// the control plane's current answer rather than against a transition it might
// have missed.
func (r *StorageNodeOpsReconciler) await(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (bool, error) {
	clusterID, nodeID, err := r.target(ctx, ops)
	if err != nil {
		return false, err
	}
	reading, err := r.nodeReading(ctx, clusterID, nodeID)
	if err != nil {
		return false, err
	}

	wanted, ok := completionStatus[ops.Spec.Action]
	if !ok {
		return false, fatalf("action %s declares no completion condition", ops.Spec.Action)
	}
	return reading.Status == wanted, nil
}

// completionStatus is what each single-step action waits for the node to report.
// The values are the control plane's own status strings, because a backend status
// is its vocabulary rather than this group's (§7.3).
var completionStatus = map[simplyblockv1alpha2.StorageNodeOpsAction]string{
	simplyblockv1alpha2.StorageNodeOpsActionShutdown: nodeStatusOffline,
	simplyblockv1alpha2.StorageNodeOpsActionRestart:  nodeStatusOnline,
	simplyblockv1alpha2.StorageNodeOpsActionSuspend:  nodeStatusSuspended,
	simplyblockv1alpha2.StorageNodeOpsActionResume:   nodeStatusOnline,
}

// boolValue reads an optional flag, absent meaning false. Both flags this is used
// for are modifiers the control plane defaults itself, so not sending one is not
// the same as sending false — which is why the spec fields are pointers and only
// the value that was stated travels.
func boolValue(v *bool) bool { return v != nil && *v }
