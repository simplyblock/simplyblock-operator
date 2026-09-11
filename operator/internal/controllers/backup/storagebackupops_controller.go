// The StorageBackupOps reconciler: it drives one operation against one backup to
// a terminal phase and leaves the object behind as the audit record.
//
// Two machines run, not one. The outer phase — Pending, Running, and the three
// terminal values — is identical for every action, so folding it into each
// action's graph would copy that spine once per action and a later fix would land
// in one copy (design-crd-model.md §3.1). The inner one is the action's steps,
// declared in restore.go.
//
// Nothing here blocks. One reconcile advances at most one step: it asks whether
// the current step has finished, and either requeues or writes the next step down
// and enters it. A step that has not finished is waiting on something outside
// this process, and waiting for it inline would hold a worker for as long as an
// S3 transfer takes.

package backup

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

const (
	// opsFinalizer is what guarantees the target's lock is released even when
	// the object is deleted mid-flight. Without it a `kubectl delete` on a
	// running restore would leave the backup locked by an object that no longer
	// exists, and nothing would ever unlock it.
	opsFinalizer = "storage.simplyblock.io/storagebackupops-finalizer"

	// opsRetry is how long an operation waits before looking again at something
	// it cannot hurry: a lock another operation holds, or a step waiting on the
	// control plane. A queued operation is normally woken by its target rather
	// than by this, and this is the backstop for when that event is missed.
	opsRetry = 15 * time.Second

	// opsAdvance is how long a pass that moved the operation forward waits
	// before the next one. It is short because there is nothing to wait for: the
	// status write this pass made is itself a change the controller watches, so
	// this is the backstop for the event rather than the path the next step
	// normally arrives on.
	opsAdvance = time.Second

	// RestoredByLabel names the operation that produced a claim. It is how the
	// Binding step recognizes its own work after a restart: the claim carries no
	// owner reference back to the operation, deliberately, so the name in
	// status.claimName and this label together are what separate a claim this
	// operation created from one somebody else did.
	RestoredByLabel = "storage.simplyblock.io/restored-by"
)

// StorageBackupOpsReconciler reconciles a StorageBackupOps.
type StorageBackupOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	API      RestoreClient
}

// RestoreClient is the control-plane surface a restore needs: the copy back,
// whether the volume it produced is usable yet, and how to reach it.
//
// It is an interface so that a test can drive the whole graph without an HTTP
// server, and it is declared here rather than in atlas-lib because what a
// restore needs is the operator's question rather than the client's.
type RestoreClient interface {
	RestoreBackup(ctx context.Context, clusterID string, params controlplane.RestoreBackupParams) (string, error)
	Volume(ctx context.Context, handle lvol.VolumeHandle) (lvol.Volume, error)
	Connection(ctx context.Context, handle lvol.VolumeHandle, opts ...lvol.ConnectionOption) (lvol.Connection, error)

	// ListVolumes is how a restarted Restoring step finds the volume a previous
	// pass created but never recorded, and DeleteVolume is how an abort takes
	// one back. Without the pair, a restore accepted by the control plane and
	// then interrupted leaves a volume nothing in Kubernetes accounts for.
	ListVolumes(ctx context.Context, clusterID, poolID string) ([]lvol.Volume, error)
	DeleteVolume(ctx context.Context, handle lvol.VolumeHandle) error
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackupops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackupops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackupops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagebackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagepools;storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager registers the controller and maps an event on a StorageBackup
// back to every operation targeting it.
//
// That mapping is what makes the queue move. An operation waiting on a lock has
// nothing of its own to react to, so a controller watching only its own kind
// would leave every queued operation waiting out a requeue interval after the
// lock frees (design-crd-model.md §3.2).
func (r *StorageBackupOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageBackupOps{}).
		Named("storagebackupops").
		Watches(&simplyblockv1alpha2.StorageBackup{},
			handler.EnqueueRequestsFromMapFunc(r.operationsOn)).
		Complete(r)
}

// operationsOn enqueues every operation naming this backup.
func (r *StorageBackupOpsReconciler) operationsOn(
	ctx context.Context, backup client.Object,
) []reconcile.Request {
	var operations simplyblockv1alpha2.StorageBackupOpsList
	if err := r.List(ctx, &operations, client.InNamespace(backup.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range operations.Items {
		if operations.Items[i].Spec.BackupRef != backup.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&operations.Items[i]),
		})
	}
	return requests
}

func (r *StorageBackupOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ops simplyblockv1alpha2.StorageBackupOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ops.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, &ops)
	}

	if !controllerutil.ContainsFinalizer(&ops, opsFinalizer) {
		controllerutil.AddFinalizer(&ops, opsFinalizer)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	// A terminal operation is a record, and a record does nothing. Releasing the
	// lock here as well as on the transition is what covers the pass that
	// crashed between the two.
	if terminal(ops.Status.Phase) {
		return ctrl.Result{}, r.releaseLock(ctx, &ops)
	}

	acquired, err := r.acquireLock(ctx, &ops)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		return ctrl.Result{RequeueAfter: opsRetry}, nil
	}

	return r.advance(ctx, &ops)
}

// advance runs the action's machine forward by at most one step.
func (r *StorageBackupOpsReconciler) advance(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (ctrl.Result, error) {
	machine, err := restoreGraph().FromSnapshot(ctx,
		statemachine.Action(ops.Spec.Action),
		statemachine.FromKube[step](ops.Status.Step))
	if err != nil {
		// An unrecognized step or action is a downgrade, a hand-edited object,
		// or a rename that shipped without a conversion, and none of them
		// resolve by reconciling again. The operation is terminal with what was
		// found in status.message, which leaves an audit record saying so.
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageBackupOpsPhaseFailed,
			fmt.Sprintf("the operation cannot be resumed: %v", err))
	}
	defer machine.Close()

	// A machine is born already in its initial state, so that state's entry hook
	// never runs and its deadline is never armed. Arming it on the first pass is
	// what stops the first step being the one step that cannot time out.
	if ops.Status.Step.State == "" {
		return r.enterInitialStep(ctx, ops, machine)
	}

	current := machine.CurrentState()

	if ops.Spec.Abort {
		return r.unwind(ctx, ops, current)
	}

	if machine.TimeoutReached() {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
			ReasonStepDeadlineExceeded, ReasonStepDeadlineExceeded,
			"Step %s outlived its deadline", current)
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageBackupOpsPhaseFailed,
			fmt.Sprintf("step %s outlived its deadline", current))
	}

	done, err := r.perform(ctx, ops, current)
	if err != nil {
		var fatal *terminalStepError
		if errors.As(err, &fatal) {
			return r.finish(ctx, ops, simplyblockv1alpha2.StorageBackupOpsPhaseFailed, fatal.Error())
		}
		logf.FromContext(ctx).Error(err, "the step could not be advanced",
			"operation", ops.Name, "step", current)
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
	}
	if !done {
		return r.waitOn(machine), r.note(ctx, ops, fmt.Sprintf("waiting on %s", current))
	}

	if machine.IsTerminal() {
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded,
			fmt.Sprintf("The backup was restored into claim %s", ops.Status.ClaimName))
	}

	next := firstSuccessor(machine)
	// Write-ahead: the step is recorded before it is entered, so a crash between
	// the two leaves a record that the step was attempted rather than a record
	// that it was not.
	if err := r.recordStep(ctx, ops, next, nil); err != nil {
		return ctrl.Result{}, err
	}
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	snapshot := statemachine.ToKube(machine.Snapshot())
	return ctrl.Result{RequeueAfter: opsAdvance}, r.recordStep(ctx, ops, next, snapshot.Deadline)
}

// enterInitialStep sets the first step's deadline and moves the operation to
// Running.
func (r *StorageBackupOpsReconciler) enterInitialStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	machine *statemachine.Machine[step],
) (ctrl.Result, error) {
	deadline := metav1.NewTime(time.Now().Add(initialDeadline))
	if err := r.recordStep(ctx, ops, machine.CurrentState(), &deadline); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
		ReasonOperationStarted, ReasonOperationStarted,
		"The operation acquired the lock on backup %s and started", ops.Spec.BackupRef)
	return ctrl.Result{RequeueAfter: opsAdvance}, nil
}

// unwind honors spec.abort where the graph allows it, and reports an abort that
// arrived too late rather than half-undoing the work.
//
// The refusal is the point. A step with no abort edge has already produced
// something the graph cannot take back, and stopping there would leave the
// system holding it with no record of what it is for.
func (r *StorageBackupOpsReconciler) unwind(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps, current step,
) (ctrl.Result, error) {
	if !abortable(current) {
		// Not a failure of the operation: it carries on. What the user asked for
		// cannot be done, and saying so is the whole of the response.
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, fmt.Sprintf(
			"the abort arrived at step %s, which has already created a volume and cannot be undone; "+
				"the operation is running on", current))
	}

	// Aborting from Restoring is the one abort that has something to take back.
	// The step asks the control plane for a volume, so an abort observed after
	// that request was accepted has to delete what it produced: reaching Aborted
	// with a volume still filling would leave the copy being written into
	// storage nothing will ever claim, read, or remove.
	if err := r.discardRestoredVolume(ctx, ops); err != nil {
		// Not terminal. The operation stays where it is and the abort is honored
		// on a later pass, because ending it now is what leaks the volume.
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops,
			fmt.Sprintf("the abort is waiting on the restored volume being discarded: %v", err))
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
		ReasonOperationAborted, ReasonOperationAborted,
		"The operation was aborted at step %s and unwound", current)
	return r.finish(ctx, ops, simplyblockv1alpha2.StorageBackupOpsPhaseAborted,
		fmt.Sprintf("aborted at step %s", current))
}

// discardRestoredVolume deletes the volume this operation asked the control
// plane for, where it got one. It is idempotent: a volume that is already gone
// is the state being asked for, and the control-plane client reads a 404 as
// success.
//
// An operation that recorded no volume may still have created one, because the
// request is made before the identifier is written down. That case is recovered
// by name rather than left: the volume carries this operation's UID, so a
// listing finds it even when the status does not.
func (r *StorageBackupOpsReconciler) discardRestoredVolume(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) error {
	volumeID := ops.Status.RestoredLvolID
	if volumeID == "" {
		recovered, err := r.restoredVolumeByName(ctx, ops)
		if err != nil {
			return err
		}
		if recovered == "" {
			return nil // nothing was ever created
		}
		volumeID = recovered
	}

	handle := lvol.NewVolumeHandle(ops.Status.ClusterID, ops.Status.PoolUUID, volumeID)
	if err := r.API.DeleteVolume(ctx, handle); err != nil {
		return fmt.Errorf("discard the restored volume %s: %w", volumeID, err)
	}
	return nil
}

// restoredVolumeByName finds the volume a previous pass created for this
// operation without recording it, and returns the empty string when there is
// none.
//
// The lookup is by the deterministic name the restore was asked for, which is
// what makes the request recoverable at all: the control plane's restore
// endpoint creates a volume every time it is called, so a retry that could not
// tell whether the first call landed would create a second one.
func (r *StorageBackupOpsReconciler) restoredVolumeByName(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (string, error) {
	if ops.Status.ClusterID == "" || ops.Status.PoolUUID == "" {
		// Validating has not resolved the pool yet, so no restore can have been
		// asked for and there is nothing to find.
		return "", nil
	}

	volumes, err := r.API.ListVolumes(ctx, ops.Status.ClusterID, ops.Status.PoolUUID)
	if err != nil {
		return "", fmt.Errorf("look for a volume already restored for %s: %w", ops.Name, err)
	}
	wanted := restoredVolumeName(ops)
	for _, volume := range volumes {
		if volume.Name != wanted {
			continue
		}
		_, _, volumeID, err := volume.ID.Split()
		if err != nil {
			return "", fmt.Errorf("read the identifier of volume %s: %w", wanted, err)
		}
		return volumeID.String(), nil
	}
	return "", nil
}

// waitOn requeues for whatever is left of the current step's deadline, so that a
// step with a long budget is looked at when it expires rather than on a fixed
// interval, and a step with a short one is not left waiting past it.
func (r *StorageBackupOpsReconciler) waitOn(machine *statemachine.Machine[step]) ctrl.Result {
	if remaining, bounded := machine.RequeueAfter(); bounded && remaining < opsRetry {
		return ctrl.Result{RequeueAfter: remaining}
	}
	return ctrl.Result{RequeueAfter: opsRetry}
}

// firstSuccessor is the step that follows the current one. Every graph in this
// package is a line rather than a tree, so the first edge is the only edge, and
// a graph that grows a branch will need the choice made here rather than in the
// caller.
func firstSuccessor(machine *statemachine.Machine[step]) step {
	for next := range machine.AllowedTransitions() {
		return next
	}
	return machine.CurrentState()
}

// finish writes a terminal phase, releases the target's lock, and records what
// the operation cost.
func (r *StorageBackupOpsReconciler) finish(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	phase simplyblockv1alpha2.StorageBackupOpsPhase,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		status.Phase = phase
		status.Message = message
		status.CompletedAt = &now
	}); err != nil {
		return ctrl.Result{}, err
	}

	switch phase {
	case simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded:
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
			ReasonOperationSucceeded, ReasonOperationSucceeded, "%s", message)
	case simplyblockv1alpha2.StorageBackupOpsPhaseFailed:
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
			ReasonOperationFailed, ReasonOperationFailed, "%s", message)
	}

	r.observeOperation(ops, phase)
	return ctrl.Result{}, r.releaseLock(ctx, ops)
}

// observeOperation records the operation's outcome and, for one that ran, how
// long it took. The duration is the recovery time objective, measured: it is the
// only number in this band that says what getting data back costs.
func (r *StorageBackupOpsReconciler) observeOperation(
	ops *simplyblockv1alpha2.StorageBackupOps, phase simplyblockv1alpha2.StorageBackupOpsPhase,
) {
	action := string(ops.Spec.Action)
	backupOperationsTotal.WithLabelValues(ops.Spec.ClusterRef, action, resultOf(phase)).Inc()

	if started := ops.Status.StartedAt; started != nil {
		backupOperationDurationSeconds.WithLabelValues(ops.Spec.ClusterRef, action).
			Observe(time.Since(started.Time).Seconds())
	}
}

// resultOf is the metric label for a terminal phase, lowercased because a label
// value is not an API enum.
func resultOf(phase simplyblockv1alpha2.StorageBackupOpsPhase) string {
	switch phase {
	case simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded:
		return "succeeded"
	case simplyblockv1alpha2.StorageBackupOpsPhaseAborted:
		return "aborted"
	default:
		return "failed"
	}
}

// teardown unwinds what the operation created, releases the lock, and lets the
// object go.
//
// The admission webhook refuses a delete from the steps where the work cannot be
// taken back, so what reaches here is either an operation that produced nothing
// or one whose product this can discard. The discard is the half that used to be
// missing: deleting an operation that had asked for a restore removed the only
// record naming the volume, and nothing afterward could find it.
//
// A terminal operation is left alone. Its record is the point, and a Succeeded
// restore's volume belongs to the claim it was bound to rather than to this
// object, which is the whole reason that claim carries no owner reference back.
func (r *StorageBackupOpsReconciler) teardown(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ops, opsFinalizer) {
		return ctrl.Result{}, nil
	}

	if !terminal(ops.Status.Phase) {
		if err := r.discardRestoredVolume(ctx, ops); err != nil {
			// Holding the object open is the point: the finalizer is what keeps
			// the volume findable, and releasing it now would lose it.
			logf.FromContext(ctx).Error(err, "the restored volume could not be discarded",
				"operation", ops.Name)
			return ctrl.Result{RequeueAfter: opsRetry}, nil
		}
	}

	if err := r.releaseLock(ctx, ops); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(ops, opsFinalizer)
	return ctrl.Result{}, r.Update(ctx, ops)
}

// acquireLock takes the target's status.activeOpsRef, and reports whether this
// operation now holds it.
//
// Acquisition is an optimistic-lock patch rather than a plain one, which is what
// makes the read-then-write safe: two operations can both read an empty field
// and both conclude the lock is free, and the patch succeeds for exactly one of
// them at a given resourceVersion and returns 409 to the rest.
func (r *StorageBackupOpsReconciler) acquireLock(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (bool, error) {
	var backup simplyblockv1alpha2.StorageBackup
	err := r.Get(ctx, client.ObjectKey{Name: ops.Spec.BackupRef, Namespace: ops.Namespace}, &backup)
	if apierrors.IsNotFound(err) {
		// The target went while the operation was waiting. That is the operation
		// stopping without going wrong, which is what Aborted means.
		_, err := r.finish(ctx, ops, simplyblockv1alpha2.StorageBackupOpsPhaseAborted,
			fmt.Sprintf("StorageBackup %s no longer exists", ops.Spec.BackupRef))
		return false, err
	}
	if err != nil {
		return false, err
	}

	if held := backup.Status.ActiveOpsRef; held != "" && held != ops.Name {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
			ReasonOperationQueued, ReasonOperationQueued,
			"Backup %s is held by operation %s; this one is waiting", backup.Name, held)
		return false, r.hold(ctx, ops, fmt.Sprintf("waiting for operation %s to release backup %s",
			held, backup.Name))
	}

	if backup.Status.ActiveOpsRef != ops.Name {
		patch := client.MergeFromWithOptions(backup.DeepCopy(), client.MergeFromWithOptimisticLock{})
		backup.Status.ActiveOpsRef = ops.Name
		if err := r.Status().Patch(ctx, &backup, patch); err != nil {
			if apierrors.IsConflict(err) {
				// Somebody else moved the object between the read and the write.
				// Whether that was another operation taking the lock is decided
				// by reading it again rather than guessed at here.
				return false, nil
			}
			return false, fmt.Errorf("acquire the lock on backup %s: %w", backup.Name, err)
		}
	}

	if ops.Status.Phase == "" || ops.Status.Phase == simplyblockv1alpha2.StorageBackupOpsPhasePending {
		now := metav1.Now()
		if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
			status.Phase = simplyblockv1alpha2.StorageBackupOpsPhaseRunning
			status.StartedAt = &now
			status.Message = "The operation holds the backup and is running"
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// releaseLock clears the target's status.activeOpsRef, but only while it still
// names this operation.
//
// The ownership check is what makes a late release safe. A pass that started
// before the lock changed hands would otherwise clear a lock somebody else now
// holds, which is worse than not releasing at all: two operations would then be
// running against one backup with neither of them knowing.
func (r *StorageBackupOpsReconciler) releaseLock(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) error {
	var backup simplyblockv1alpha2.StorageBackup
	err := r.Get(ctx, client.ObjectKey{Name: ops.Spec.BackupRef, Namespace: ops.Namespace}, &backup)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if backup.Status.ActiveOpsRef != ops.Name {
		return nil
	}

	patch := client.MergeFromWithOptions(backup.DeepCopy(), client.MergeFromWithOptimisticLock{})
	backup.Status.ActiveOpsRef = ""
	if err := r.Status().Patch(ctx, &backup, patch); err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("release the lock on backup %s: %w", backup.Name, err)
	}
	return nil
}

// hold reports an operation that is admitted, holds nothing, and is waiting.
// Pending is both where an operation starts and where it waits, and the
// OperationQueued event is the only thing that separates the two.
func (r *StorageBackupOpsReconciler) hold(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		if status.Phase == "" {
			status.Phase = simplyblockv1alpha2.StorageBackupOpsPhasePending
		}
		status.Message = message
	})
}

// note replaces status.message without moving anything else. It is one sentence
// about where the operation is, replaced as it moves, and never a log.
func (r *StorageBackupOpsReconciler) note(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		status.Message = message
	})
}

// recordStep persists the step the operation is about to be in, with the instant
// it expires. Both travel together, because a step persisted without its
// deadline restores as a step that can never time out.
func (r *StorageBackupOpsReconciler) recordStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	next step,
	deadline *metav1.Time,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageBackupOpsStatus) {
		status.Phase = simplyblockv1alpha2.StorageBackupOpsPhaseRunning
		status.Step = statemachine.KubeSnapshot{State: string(next), Deadline: deadline}
	})
}

// writeStatus applies the mutation and patches only when something changed.
//
// observedGeneration is written here rather than by each caller. On this kind it
// advances at most twice, and the second advance is precisely the signal that
// spec.abort has been observed: a user who sets it and sees an unchanged status
// cannot otherwise tell a controller that has not looked from one that looked
// and declined.
func (r *StorageBackupOpsReconciler) writeStatus(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageBackupOps,
	mutate func(*simplyblockv1alpha2.StorageBackupOpsStatus),
) error {
	// Retried rather than swallowed on a conflict. A caller that read nil would
	// take the write for done, and finish does: it releases the target's lock
	// straight afterward, so a dropped terminal status would free the backup for
	// the next restore while this operation still reported Running.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.StorageBackupOps
		if err := r.Get(ctx, client.ObjectKeyFromObject(ops), &fresh); err != nil {
			return err
		}

		desired := *fresh.Status.DeepCopy()
		mutate(&desired)
		desired.ObservedGeneration = fresh.Generation

		if reflect.DeepEqual(fresh.Status, desired) {
			// Still published to the caller, which reads the object it passed in
			// on the next line of its own logic.
			ops.Status = desired
			ops.ResourceVersion = fresh.ResourceVersion
			return nil
		}

		patch := client.MergeFromWithOptions(fresh.DeepCopy(), client.MergeFromWithOptimisticLock{})
		fresh.Status = desired
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		ops.Status = fresh.Status
		ops.ResourceVersion = fresh.ResourceVersion
		return nil
	})
}

// terminal reports a phase the operation can never leave.
func terminal(phase simplyblockv1alpha2.StorageBackupOpsPhase) bool {
	switch phase {
	case simplyblockv1alpha2.StorageBackupOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageBackupOpsPhaseFailed,
		simplyblockv1alpha2.StorageBackupOpsPhaseAborted:
		return true
	default:
		return false
	}
}

// terminalStepError is a step failure that retrying cannot fix: a claim that
// already exists, a pool this cluster does not have, a backup whose copy failed.
// It is a distinct type so that the reconcile loop can tell it from a control
// plane that is briefly unreachable, which is the same shape of error and the
// opposite response.
type terminalStepError struct{ reason string }

func (e *terminalStepError) Error() string { return e.reason }

func fatalf(format string, args ...any) error {
	return &terminalStepError{reason: fmt.Sprintf(format, args...)}
}

// clusterIDFor resolves the operation's cluster once, and is separate only
// because three steps need it.
func (r *StorageBackupOpsReconciler) clusterIDFor(
	ctx context.Context, ops *simplyblockv1alpha2.StorageBackupOps,
) (string, error) {
	if recorded := ops.Status.ClusterID; recorded != "" {
		return recorded, nil
	}
	return utils.ResolveClusterUUID(ctx, r.Client, ops.Namespace, ops.Spec.ClusterRef)
}
