// The StoragePoolOps reconciler: the lock, the phases, and the finalizer that
// every Ops kind in this group has.
//
// What it does not have is an action that does anything, and that is the design
// rather than an omission. A pool has fewer operations than a node because most
// of what changes about a pool is desired state: raising its capacity is an
// edit, restricting its nodes is an edit, and the node list is resolved on every
// reconcile of the pool itself. Rebalance is the one candidate with a real
// motivation — narrowing spec.allowedNodes stops new volumes landing on the
// removed nodes and does nothing to the volumes already there — and it is kept
// declared, and refused at run time, because the gap it would close is worth
// remembering and building it now would be building against a fan-out
// (PersistentVolumeOps) that does not exist yet.
//
// So this file is the shape, built and tested: an operation takes its target's
// lock, releases it on every terminal path including deletion, and says clearly
// why it did not run. When the fan-out lands, what it plugs into is here.
//
// design-storagepool.md §7 is the specification.

package pool

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// FinalizerStoragePoolOps is what guarantees the lock is released even when the
// operation is deleted while it holds one. Without it, deleting a Running
// operation would leave its pool locked against every later one, with nothing
// left in the cluster to say why.
const FinalizerStoragePoolOps = "storage.simplyblock.io/storagepoolops-finalizer"

// requeueLocked is how long an operation waits before asking for the lock again.
const requeueLocked = 15 * time.Second

// StoragePoolOpsReconciler reconciles a StoragePoolOps.
type StoragePoolOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepoolops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepoolops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepoolops/finalizers,verbs=update

// Reconcile drives one operation.
func (r *StoragePoolOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ops := &simplyblockv1alpha2.StoragePoolOps{}
	if err := r.Get(ctx, req.NamespacedName, ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ops.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, ops)
	}

	// A terminal operation is a record rather than a task. Re-reconciling one
	// does nothing at all, including nothing to its target: an operation that
	// finished has already released the lock, and taking it again to release it
	// again is how an unrelated operation loses one it legitimately holds.
	if terminal(ops.Status.Phase) {
		return ctrl.Result{}, r.ensureFinalizer(ctx, ops)
	}

	if err := r.ensureFinalizer(ctx, ops); err != nil {
		return ctrl.Result{}, err
	}

	target := &simplyblockv1alpha2.StoragePool{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ops.Namespace, Name: ops.Spec.PoolRef}, target)
	switch {
	case apierrors.IsNotFound(err):
		return r.finish(ctx, ops, nil, simplyblockv1alpha2.StoragePoolOpsPhaseFailed,
			fmt.Sprintf("no StoragePool %q in namespace %s", ops.Spec.PoolRef, ops.Namespace))
	case err != nil:
		return ctrl.Result{}, err
	}

	if ops.Spec.Abort {
		// There is no step in flight to unwind, because no action runs, so an
		// abort is immediate. When an action is built, this is where its unwind
		// goes.
		return r.abort(ctx, ops, target)
	}

	if ops.Status.Phase != simplyblockv1alpha2.StoragePoolOpsPhaseRunning {
		acquired, result, err := r.acquireLock(ctx, ops, target)
		if err != nil || !acquired {
			return result, err
		}
	}

	return r.run(ctx, ops, target)
}

// acquireLock takes the target's status.activeOpsRef, which is the mutual
// exclusion every Ops kind in this group uses. An operation that finds the lock
// held by another stays Pending and asks again, rather than failing: the other
// operation will finish, and failing here would make the order two people
// applied two objects in decide which of them runs.
func (r *StoragePoolOpsReconciler) acquireLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.StoragePoolOps,
	target *simplyblockv1alpha2.StoragePool,
) (bool, ctrl.Result, error) {
	if held := target.Status.ActiveOpsRef; held != "" && held != ops.Name {
		if ops.Status.Phase != simplyblockv1alpha2.StoragePoolOpsPhasePending {
			if err := r.setPhase(ctx, ops, simplyblockv1alpha2.StoragePoolOpsPhasePending,
				fmt.Sprintf("waiting for operation %q to release the pool", held)); err != nil {
				return false, ctrl.Result{}, err
			}
		}
		r.event(ops, corev1.EventTypeNormal, OperationQueued,
			"pool %q is held by operation %q", target.Name, held)
		return false, ctrl.Result{RequeueAfter: requeueLocked}, nil
	}

	// The optimistic lock is what makes this a lock at all: two reconcilers that
	// both saw a free field patch the same resourceVersion, and one of them gets
	// a 409 and comes back to find the field taken.
	base := target.DeepCopy()
	target.Status.ActiveOpsRef = ops.Name
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := r.Status().Patch(ctx, target, patch); err != nil {
		return false, ctrl.Result{RequeueAfter: requeueContended}, nil //nolint:nilerr // a lost race is retried, not failed
	}

	now := metav1.Now()
	opsBase := ops.DeepCopy()
	ops.Status.Phase = simplyblockv1alpha2.StoragePoolOpsPhaseRunning
	ops.Status.Step.State = string(simplyblockv1alpha2.StoragePoolOpsStepValidating)
	ops.Status.StartedAt = &now
	ops.Status.Message = ""
	ops.Status.ObservedGeneration = ops.Generation
	if err := r.Status().Patch(ctx, ops, client.MergeFrom(opsBase)); err != nil {
		return false, ctrl.Result{}, err
	}
	r.event(ops, corev1.EventTypeNormal, OperationStarted,
		"acquired pool %q and started %s", target.Name, ops.Spec.Action)
	return true, ctrl.Result{Requeue: true}, nil
}

// run dispatches the action. Rebalance is declared and not implemented, so what
// it does is say so, once, in a terminal phase — which is the honest outcome and
// not the same as an unknown action.
func (r *StoragePoolOpsReconciler) run(
	ctx context.Context,
	ops *simplyblockv1alpha2.StoragePoolOps,
	target *simplyblockv1alpha2.StoragePool,
) (ctrl.Result, error) {
	switch ops.Spec.Action {
	case simplyblockv1alpha2.StoragePoolOpsActionRebalance:
		return r.failAndRelease(ctx, ops, target,
			"action Rebalance is declared but not implemented: it is a fan-out of one "+
				"PersistentVolumeOps per misplaced volume, and that kind does not exist yet")
	default:
		return r.failAndRelease(ctx, ops, target,
			fmt.Sprintf("unknown action %q", ops.Spec.Action))
	}
}

// abort stops an operation on request. Nothing is in flight to unwind, so the
// phase moves straight to Aborted and the lock is released.
func (r *StoragePoolOpsReconciler) abort(
	ctx context.Context,
	ops *simplyblockv1alpha2.StoragePoolOps,
	target *simplyblockv1alpha2.StoragePool,
) (ctrl.Result, error) {
	if err := r.releaseLock(ctx, ops, target); err != nil {
		return ctrl.Result{}, err
	}
	r.event(ops, corev1.EventTypeNormal, OperationAborted,
		"aborted on request; nothing was in flight to unwind")
	return r.finish(ctx, ops, target, simplyblockv1alpha2.StoragePoolOpsPhaseAborted, "aborted on request")
}

func (r *StoragePoolOpsReconciler) failAndRelease(
	ctx context.Context,
	ops *simplyblockv1alpha2.StoragePoolOps,
	target *simplyblockv1alpha2.StoragePool,
	message string,
) (ctrl.Result, error) {
	if err := r.releaseLock(ctx, ops, target); err != nil {
		return ctrl.Result{}, err
	}
	r.event(ops, corev1.EventTypeWarning, OperationFailed, "%s", message)
	return r.finish(ctx, ops, target, simplyblockv1alpha2.StoragePoolOpsPhaseFailed, message)
}

// releaseLock clears the target's lock, and only when this operation is the one
// holding it. The guard is the whole of the safety: an operation that never
// acquired the lock, or whose lock was taken over, must not clear somebody
// else's.
func (r *StoragePoolOpsReconciler) releaseLock(
	ctx context.Context,
	ops *simplyblockv1alpha2.StoragePoolOps,
	target *simplyblockv1alpha2.StoragePool,
) error {
	if target == nil || target.Status.ActiveOpsRef != ops.Name {
		return nil
	}
	base := target.DeepCopy()
	target.Status.ActiveOpsRef = ""
	if err := r.Status().Patch(ctx, target, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("release the lock on pool %s/%s: %w", target.Namespace, target.Name, err)
	}
	return nil
}

// reconcileDeletion releases the lock the operation may still hold, then lets the
// object go. This is the path that matters most: an operation deleted while
// Running has taken a lock that nothing else would ever clear.
func (r *StoragePoolOpsReconciler) reconcileDeletion(
	ctx context.Context, ops *simplyblockv1alpha2.StoragePoolOps,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ops, FinalizerStoragePoolOps) {
		return ctrl.Result{}, nil
	}

	target := &simplyblockv1alpha2.StoragePool{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ops.Namespace, Name: ops.Spec.PoolRef}, target)
	switch {
	case apierrors.IsNotFound(err):
		// The pool went first, so there is no lock left to release.
	case err != nil:
		return ctrl.Result{}, err
	default:
		if err := r.releaseLock(ctx, ops, target); err != nil {
			return ctrl.Result{}, err
		}
	}

	base := ops.DeepCopy()
	controllerutil.RemoveFinalizer(ops, FinalizerStoragePoolOps)
	if err := r.Patch(ctx, ops, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("release the finalizer on operation %s/%s: %w",
			ops.Namespace, ops.Name, err)
	}
	return ctrl.Result{}, nil
}

func (r *StoragePoolOpsReconciler) ensureFinalizer(
	ctx context.Context, ops *simplyblockv1alpha2.StoragePoolOps,
) error {
	if controllerutil.ContainsFinalizer(ops, FinalizerStoragePoolOps) {
		return nil
	}
	base := ops.DeepCopy()
	controllerutil.AddFinalizer(ops, FinalizerStoragePoolOps)
	return r.Patch(ctx, ops, client.MergeFrom(base))
}

// finish writes a terminal phase and the time it was reached, and records the
// operation in the two cumulative series.
//
// target may be nil, which is the case where the pool the operation named does
// not exist. The operation is still counted, under an empty pool label, because
// an operation that failed for that reason is exactly the kind somebody wants to
// see a rate of.
func (r *StoragePoolOpsReconciler) finish(
	ctx context.Context,
	ops *simplyblockv1alpha2.StoragePoolOps,
	target *simplyblockv1alpha2.StoragePool,
	phase simplyblockv1alpha2.StoragePoolOpsPhase,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	base := ops.DeepCopy()
	ops.Status.Phase = phase
	ops.Status.Message = message
	ops.Status.CompletedAt = &now
	ops.Status.ObservedGeneration = ops.Generation
	if err := r.Status().Patch(ctx, ops, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	if phase == simplyblockv1alpha2.StoragePoolOpsPhaseSucceeded {
		r.event(ops, corev1.EventTypeNormal, OperationSucceeded, "%s", message)
	}
	recordOperation(ops, target, phase)
	return ctrl.Result{}, nil
}

// recordOperation adds one terminal operation to the counter, and its duration
// to the histogram when the operation got far enough to have one. An operation
// that never started has no duration to report, and reporting zero would drag
// the distribution toward a value nothing took.
func recordOperation(
	ops *simplyblockv1alpha2.StoragePoolOps,
	target *simplyblockv1alpha2.StoragePool,
	phase simplyblockv1alpha2.StoragePoolOpsPhase,
) {
	var cluster, name string
	if target != nil {
		cluster, name = target.Spec.ClusterRef, target.Name
	}
	action := string(ops.Spec.Action)
	poolOperationsTotal.WithLabelValues(cluster, name, action, string(phase)).Inc()
	if ops.Status.StartedAt != nil && ops.Status.CompletedAt != nil {
		seconds := ops.Status.CompletedAt.Sub(ops.Status.StartedAt.Time).Seconds()
		poolOperationDuration.WithLabelValues(cluster, name, action).Observe(seconds)
	}
}

func (r *StoragePoolOpsReconciler) setPhase(
	ctx context.Context,
	ops *simplyblockv1alpha2.StoragePoolOps,
	phase simplyblockv1alpha2.StoragePoolOpsPhase,
	message string,
) error {
	base := ops.DeepCopy()
	ops.Status.Phase = phase
	ops.Status.Message = message
	ops.Status.ObservedGeneration = ops.Generation
	return r.Status().Patch(ctx, ops, client.MergeFrom(base))
}

func (r *StoragePoolOpsReconciler) event(
	object client.Object, eventType, reason, format string, args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(object, nil, eventType, reason, reason, format, args...)
}

// terminal reports whether a phase is one the operation never leaves.
func terminal(phase simplyblockv1alpha2.StoragePoolOpsPhase) bool {
	switch phase {
	case simplyblockv1alpha2.StoragePoolOpsPhaseSucceeded,
		simplyblockv1alpha2.StoragePoolOpsPhaseFailed,
		simplyblockv1alpha2.StoragePoolOpsPhaseAborted:
		return true
	default:
		return false
	}
}

// SetupWithManager registers the reconciler.
func (r *StoragePoolOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StoragePoolOps{}).
		Named("storagepoolops").
		Complete(r)
}
