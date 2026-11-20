// The Awaiting step of an Activate, and the retry of an attempt the control
// plane gave up on.
//
// It is the one completion condition in this package with a failure branch of
// its own. The control plane accepts an activation with a 202 and runs it in a
// thread, and when that thread fails it reverts the cluster to the status it had
// before the attempt and reports nothing else. Every other action's Awaiting
// waits for a reading that only success produces. An Activate that did the same
// waited out its whole deadline on a cluster nothing was activating any more
// (e2e run 36883427064), so a reverted status is read here as a lost attempt
// and the call is made again.

package cluster

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// activationSettleGrace is how long after the call a pre-activation status is
// still no evidence about the attempt. The control plane sends its 202 before
// its thread writes in_activation, and the cluster stream delivers that write
// later still, so the first readings of Awaiting can show the status the
// attempt started from.
const activationSettleGrace = 30 * time.Second

// activationRevertedError is an Awaiting pass that found the attempt over and
// the cluster not active. It is not a failure of the operation: advance answers
// it by requesting again, or by failing once the retry budget is spent.
type activationRevertedError struct {
	status string
}

func (e *activationRevertedError) Error() string {
	return fmt.Sprintf("the control plane gave up on the activation and the cluster is back at %s",
		e.status)
}

// awaitActivation is Awaiting for an Activate: done once the cluster is active,
// and an activationRevertedError once the cluster has fallen back to a status an
// activation starts from.
func (r *StorageClusterOpsReconciler) awaitActivation(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, clusterID string,
) (bool, error) {
	reading, err := r.clusterReading(ctx, clusterID)
	if err != nil {
		return false, err
	}
	if reading.Status == utils.ClusterStatusActive {
		return true, nil
	}
	if !activationReverted(reading.Status) || timeInAwaiting(ops) < activationSettleGrace {
		return false, nil
	}
	return false, &activationRevertedError{status: reading.Status}
}

// activationReverted reports whether a status is one the control plane puts a
// cluster back at when an activation fails: the status the attempt started
// from, which for an activation is unready on a new cluster and suspended on a
// re-activation.
//
// The list is closed on purpose. in_activation is an attempt still running, and
// degraded is a cluster an activation already brought up, so neither is a lost
// attempt, and activating a degraded cluster again would re-run the activation
// on a cluster that is serving.
func activationReverted(status string) bool {
	return status == utils.ClusterStatusUnready || status == utils.ClusterStatusSuspended
}

// timeInAwaiting is how long ago the operation entered Awaiting, read as the
// step's deadline minus the budget it was given, as observeStep reads it. A
// step with no deadline reads as just entered, which is the side that does not
// retry.
func timeInAwaiting(ops *simplyblockv1alpha2.StorageClusterOps) time.Duration {
	deadline, bounded := ops.Status.Step.KubeDeadline()
	if !bounded {
		return 0
	}
	return time.Since(deadline.Add(-awaitingDeadline))
}

// retryActivation answers a lost attempt by returning the machine to Requesting,
// or fails the operation once the retry budget is spent.
//
// The return is Machine.Reset rather than an edge, as the rolling restart's walk
// does it: an edge out of Awaiting would make it non-terminal, and its being
// terminal is what tells advance a completed wait is a finished operation. The
// step and its deadline are written in one patch before the call is made again,
// so a crash between the two restarts into Requesting, whose activation is
// skipped on a cluster already active.
func (r *StorageClusterOpsReconciler) retryActivation(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	machine *statemachine.Machine[step],
	reverted *activationRevertedError,
) (ctrl.Result, error) {
	// No start time is a budget nothing can measure, and failing is the side of
	// that which cannot loop.
	started := ops.Status.StartedAt
	if started == nil || time.Since(started.Time) >= activationRetryBudget {
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseFailed,
			fmt.Sprintf("%s, and the %s retry budget is spent", reverted.Error(), activationRetryBudget))
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
		ActivationRetried, ActivationRetried,
		"%s; requesting the activation again", reverted.Error())

	machine.Reset()
	deadline := metav1.NewTime(time.Now().Add(requestingDeadline))
	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.Step = statemachine.KubeSnapshot{
			State:    string(machine.CurrentState()),
			Deadline: &deadline,
		}
		status.Message = reverted.Error() + "; requesting the activation again"
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: opsAdvance}, nil
}
