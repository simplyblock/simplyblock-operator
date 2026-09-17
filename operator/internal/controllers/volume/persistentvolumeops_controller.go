// The PersistentVolumeOps reconciler: it drives one migration of one volume to
// a terminal phase and leaves the object behind as the audit record.
//
// Two machines run, not one. The outer phase — Pending, Running, and the three
// terminal values — is identical for every action, so folding it into the
// action's graph would copy that spine once per action and a later fix would
// land in one copy (design-crd-model.md §3.1). The inner one is the action's
// steps, declared in graphs.go.
//
// Nothing here blocks. One reconcile advances at most one step: it asks whether
// the current step has finished, and either requeues or writes the next step
// down and enters it. A step that has not finished is waiting on something
// outside this process — a data copy, a Job on another node — and waiting for
// it inline would hold a worker for as long as the copy takes.
//
// The registered VolumeMigration reconciler runs beside this one. The two never
// act on the same object: they reconcile different kinds, and nothing creates
// one of each for a volume. What they do share is the volume, and this one
// alone takes a lock on it, which is the cost of the coexistence and the reason
// in-flight migrations are drained on the old kind rather than converted.
//
// design-persistentvolumeops.md is the specification.

package volume

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/driver"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

const (
	// opsFinalizer is what stops an operation deleted mid-flight from leaving
	// the volume locked and its validation Jobs running with nothing left to
	// account for them. The admission guard refuses a delete from the one step
	// where the work cannot be taken back; what reaches the finalizer is an
	// operation whose product this can unwind.
	opsFinalizer = "storage.simplyblock.io/persistentvolumeops-finalizer"

	// opsRetry is how long an operation waits before looking again at
	// something it cannot hurry: a lock another operation holds, a control
	// plane that is not accepting migrations, or a Job still running.
	opsRetry = 15 * time.Second

	// opsAdvance is how long a pass that moved the operation forward waits
	// before the next one. It is short because there is nothing to wait for:
	// the status write this pass made is itself a change the controller
	// watches, so this is the backstop for the event rather than the path the
	// next step normally arrives on.
	opsAdvance = time.Second

	// CSIDriverName is the driver whose volumes this operator can move. A
	// deployment that renamed its driver is matched against that name instead;
	// this is the default and the answer when no SimplyblockDriver exists.
	CSIDriverName = driver.DefaultDriverName

	// PersistentVolumeNameField indexes operations by the volume they act on.
	// Releasing a lock has to wake whatever was waiting on it, and the index is
	// what makes that a lookup rather than a listing of every operation in the
	// cluster on every release.
	PersistentVolumeNameField = ".spec.persistentVolumeName"
)

// MigrationClient is the control-plane surface a migration needs.
//
// It is an interface so that a test can drive the whole graph without an HTTP
// server, and it is declared here rather than in atlas-lib because what a
// migration needs is the operator's question rather than the client's.
type MigrationClient interface {
	// Volume resolves the subsystem the migration is addressed by: the control
	// plane migrates a subsystem rather than one volume inside it.
	Volume(ctx context.Context, handle lvol.VolumeHandle) (lvol.Volume, error)

	// SubsystemVolumes is how the sibling volumes are found. Each of them has
	// a consuming host that must reach the target before the cutover, because
	// at cutover every member moves at once.
	SubsystemVolumes(ctx context.Context, clusterID, nqn string) ([]lvol.Volume, error)

	CreateMigration(ctx context.Context, clusterID, nqn, targetNodeID string) (controlplane.Migration, error)
	ContinueMigration(ctx context.Context, clusterID, nqn, migrationID string) error
	GetMigration(ctx context.Context, clusterID, nqn, migrationID string) (controlplane.Migration, error)
	CancelMigration(ctx context.Context, clusterID, nqn, migrationID string) error
}

// PersistentVolumeOpsReconciler reconciles a PersistentVolumeOps.
type PersistentVolumeOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	API      MigrationClient

	// Reader is uncached, and the consumer lookup is what it is for: a stale
	// informer cache can miss a pod that is genuinely running, and a volume
	// whose consumer was missed is one whose host never gets the target's
	// paths and loses its volume at cutover.
	Reader client.Reader
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=persistentvolumeops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=persistentvolumeops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=persistentvolumeops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters;storagenodes;simplyblockdrivers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// SetupWithManager registers the controller, the field index the queue reads,
// and the watch that wakes a queued operation when the volume's lock frees.
//
// The mapping is what makes the queue move. An operation waiting on a lock has
// nothing of its own to react to, so a controller watching only its own kind
// would leave every queued operation waiting out a requeue interval after the
// lock frees (design-crd-model.md §3.2).
func (r *PersistentVolumeOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&simplyblockv1alpha2.PersistentVolumeOps{},
		PersistentVolumeNameField,
		func(object client.Object) []string {
			ops, ok := object.(*simplyblockv1alpha2.PersistentVolumeOps)
			if !ok {
				return nil
			}
			return []string{ops.Spec.PersistentVolumeName}
		},
	); err != nil {
		return fmt.Errorf("index operations by the volume they act on: %w", err)
	}

	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.PersistentVolumeOps{}).
		Named("persistentvolumeops").
		Watches(&corev1.PersistentVolume{},
			handler.EnqueueRequestsFromMapFunc(r.operationsOn),
			// A volume's own updates are frequent and mostly about capacity and
			// phase. What this watch is for is the lock changing hands and the
			// volume being deleted, and both are visible in metadata.
			builder.WithPredicates(volumeLockChanged{})).
		Complete(r)
}

// operationsOn enqueues every operation naming this volume.
func (r *PersistentVolumeOpsReconciler) operationsOn(
	ctx context.Context, pv client.Object,
) []reconcile.Request {
	var operations simplyblockv1alpha2.PersistentVolumeOpsList
	if err := r.List(ctx, &operations,
		client.MatchingFields{PersistentVolumeNameField: pv.GetName()}); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(operations.Items))
	for i := range operations.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&operations.Items[i]),
		})
	}
	return requests
}

func (r *PersistentVolumeOpsReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var ops simplyblockv1alpha2.PersistentVolumeOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ops.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, &ops)
	}

	// A terminal operation is a record rather than a task. Releasing the lock
	// here as well as on the transition is what covers the pass that crashed
	// between the two.
	if terminal(ops.Status.Phase) {
		return ctrl.Result{}, r.releaseLock(ctx, &ops)
	}

	if !controllerutil.ContainsFinalizer(&ops, opsFinalizer) {
		controllerutil.AddFinalizer(&ops, opsFinalizer)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	subject, err := r.resolve(ctx, &ops)
	switch {
	case errors.Is(err, errVolumeGone):
		// The operation follows its volume. A claim deleted under a Delete
		// reclaim policy makes the driver delete the backing logical volume,
		// and moving a volume that is being deleted is work nobody will read.
		// Aborted rather than Failed, because a migration whose volume went
		// away did not go wrong.
		return r.abandon(ctx, &ops, err.Error())
	case err != nil:
		var fatal *terminalStepError
		if errors.As(err, &fatal) {
			r.event(&ops, corev1.EventTypeWarning, ReasonClusterUnresolvable, "%s", fatal.Error())
			return r.finish(ctx, &ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed, fatal.Error())
		}
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, &ops, err.Error())
	}

	acquired, err := r.acquireLock(ctx, &ops, subject.pv)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		held := subject.pv.Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]
		r.event(&ops, corev1.EventTypeNormal, ReasonOperationQueued,
			"Volume %s is held by operation %s; this one is waiting", subject.pv.Name, held)
		return ctrl.Result{RequeueAfter: opsRetry}, r.hold(ctx, &ops,
			fmt.Sprintf("waiting for operation %s to release volume %s", held, subject.pv.Name))
	}

	return r.advance(ctx, &ops, subject)
}

// advance runs the action's machine forward by at most one step.
func (r *PersistentVolumeOpsReconciler) advance(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject,
) (ctrl.Result, error) {
	machine, err := graphs(memberCount(ops)).FromSnapshot(ctx,
		statemachine.Action(ops.Spec.Action),
		statemachine.FromKube[step](ops.Status.Step))
	if err != nil {
		// An unrecognized step or action is a downgrade, a hand-edited object,
		// or a rename that shipped without a conversion, and none of them
		// resolve by reconciling again. The operation is terminal with what was
		// found in status.message, which leaves an audit record saying so.
		return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
			fmt.Sprintf("the operation cannot be resumed: %v", err))
	}
	defer machine.Close()

	// A machine is born already in its initial state, so that state's entry
	// hook never runs and its deadline is never armed. Arming it on the first
	// pass is what stops the first step being the one step that cannot time
	// out.
	if ops.Status.Step.State == "" {
		return r.enterInitialStep(ctx, ops, machine)
	}

	current := machine.CurrentState()

	if ops.Spec.Abort {
		return r.unwind(ctx, ops, subject, machine, current)
	}

	if machine.TimeoutReached() {
		r.event(ops, corev1.EventTypeWarning, ReasonStepDeadlineExceeded,
			"Step %s outlived its deadline", current)
		stepDeadlinesExceeded.WithLabelValues(
			subject.clusterUUID, string(ops.Spec.Action), string(current)).Inc()
		return r.fail(ctx, ops, subject, fmt.Sprintf("step %s outlived its deadline", current))
	}

	done, err := r.perform(ctx, ops, subject, current)
	if err != nil {
		var fatal *terminalStepError
		if errors.As(err, &fatal) {
			return r.fail(ctx, ops, subject, fatal.Error())
		}
		logf.FromContext(ctx).Error(err, "the step could not be advanced",
			"operation", ops.Name, "step", current)
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
	}
	if !done {
		return r.waitOn(machine), r.note(ctx, ops, fmt.Sprintf("waiting on %s", current))
	}

	r.observeStep(ops, subject, current)

	if machine.IsTerminal() {
		r.countTowardRealignment(ctx, subject)
		r.event(ops, corev1.EventTypeNormal, ReasonOperationSucceeded,
			"Volume %s was moved to node %s", ops.Spec.PersistentVolumeName, subject.targetNodeName())
		return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded,
			fmt.Sprintf("the volume was moved to node %s", subject.targetNodeName()))
	}

	next := firstSuccessor(machine)
	// Write-ahead: the step is recorded before it is entered, so a crash
	// between the two leaves a record that the step was attempted rather than a
	// record that it was not.
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
func (r *PersistentVolumeOpsReconciler) enterInitialStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	machine *statemachine.Machine[step],
) (ctrl.Result, error) {
	now := metav1.Now()
	deadline := metav1.NewTime(now.Add(initialDeadline))
	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
		status.Step = statemachine.KubeSnapshot{
			State:    string(machine.CurrentState()),
			Deadline: &deadline,
		}
		status.Message = "the operation holds the volume and is running"
		if status.StartedAt == nil {
			status.StartedAt = &now
		}
		status.DeferredSince = nil
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(ops, corev1.EventTypeNormal, ReasonOperationStarted,
		"The operation acquired the lock on volume %s and started",
		ops.Spec.PersistentVolumeName)
	return ctrl.Result{RequeueAfter: opsAdvance}, nil
}

// unwind honors spec.abort where the graph allows it, and reports an abort that
// arrived too late rather than half-undoing the work.
//
// The refusal is the point. Verifying has already cut the volume over, so an
// abort there would leave the volume moved and the paths its move created
// untracked, which is the state the step exists to prevent.
func (r *PersistentVolumeOpsReconciler) unwind(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
	machine *statemachine.Machine[step],
	current step,
) (ctrl.Result, error) {
	// The machine is asked rather than a table beside it: the graph it was
	// built from is the one authority over what this action can stop from, and
	// the DELETE guard reads the same graph.
	if !machine.CanAbort() {
		// Not a failure of the operation: it carries on. What was asked for
		// cannot be done, and saying so is the whole of the response.
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, fmt.Sprintf(
			"the abort arrived at step %s, by which point the volume has already moved "+
				"and the cleanup is what makes the move safe; the operation is running on", current))
	}

	if err := r.discardMigration(ctx, ops, subject); err != nil {
		// Not terminal. The operation stays where it is and the abort is
		// honored on a later pass, because ending it now is what leaks the
		// backend migration and the paths it published.
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops,
			fmt.Sprintf("the abort is waiting on the migration being taken back: %v", err))
	}

	r.event(ops, corev1.EventTypeNormal, ReasonOperationAborted,
		"The operation was aborted at step %s and unwound", current)
	return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted,
		fmt.Sprintf("aborted at step %s", current))
}

// fail ends the operation, having first taken back what it created. A migration
// abandoned with its target paths still connected on every consumer host is the
// defect this package exists around, and a failure is the commonest way to get
// there.
func (r *PersistentVolumeOpsReconciler) fail(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	subject *subject,
	message string,
) (ctrl.Result, error) {
	if err := r.discardMigration(ctx, ops, subject); err != nil {
		logf.FromContext(ctx).Error(err, "the migration could not be taken back",
			"operation", ops.Name)
		message += fmt.Sprintf(" (taking the migration back also failed: %v)", err)
	}
	r.event(ops, corev1.EventTypeWarning, ReasonOperationFailed, "%s", message)
	return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed, message)
}

// abandon ends an operation whose volume went away, which is a stop rather than
// a failure. Cancel first, then let the object go: a logical volume with a
// migration running against it is not one the control plane can cleanly delete.
func (r *PersistentVolumeOpsReconciler) abandon(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, message string,
) (ctrl.Result, error) {
	if err := r.discardMigration(ctx, ops, nil); err != nil {
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops,
			fmt.Sprintf("the volume is gone and the migration could not be taken back: %v", err))
	}
	r.event(ops, corev1.EventTypeNormal, ReasonOperationAborted, "%s", message)
	return r.finish(ctx, ops, simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted, message)
}

// teardown unwinds what the operation created, releases the volume's lock, and
// lets the object go.
//
// The admission guard refuses a delete from the step where the work cannot be
// taken back, so what reaches here either produced nothing or produced
// something this can discard. A finalizer that cannot finish holds the object
// open rather than dropping the paths, which is the intended outcome: a path
// connected with nothing tracking it blocks every later migration of the
// volume, and has.
func (r *PersistentVolumeOpsReconciler) teardown(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ops, opsFinalizer) {
		return ctrl.Result{}, nil
	}

	if !terminal(ops.Status.Phase) {
		if err := r.discardMigration(ctx, ops, nil); err != nil {
			r.event(ops, corev1.EventTypeWarning, ReasonCleanupBlocked,
				"The delete is held because the cleanup has not finished: %v", err)
			logf.FromContext(ctx).Error(err, "the migration could not be taken back",
				"operation", ops.Name)
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops,
				fmt.Sprintf("the delete is held until the cleanup finishes: %v", err))
		}
	}

	if err := r.releaseLock(ctx, ops); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(ops, opsFinalizer)
	return ctrl.Result{}, r.Update(ctx, ops)
}

// waitOn requeues for whatever is left of the current step's deadline, so that
// a step with a long budget is looked at when it expires rather than on a fixed
// interval, and a step with a short one is not left waiting past it.
func (r *PersistentVolumeOpsReconciler) waitOn(machine *statemachine.Machine[step]) ctrl.Result {
	if remaining, bounded := machine.RequeueAfter(); bounded && remaining < opsRetry {
		return ctrl.Result{RequeueAfter: remaining}
	}
	return ctrl.Result{RequeueAfter: opsRetry}
}

// firstSuccessor is the step that follows the current one. The graph is a line
// rather than a tree, so the first edge is the only edge, and a graph that
// grows a branch will need the choice made here rather than in the caller.
func firstSuccessor(machine *statemachine.Machine[step]) step {
	for next := range machine.AllowedTransitions() {
		return next
	}
	return machine.CurrentState()
}

// finish writes a terminal phase, releases the volume's lock, and records what
// the operation cost.
func (r *PersistentVolumeOpsReconciler) finish(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	phase simplyblockv1alpha2.PersistentVolumeOpsPhase,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	if err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Phase = phase
		status.Message = message
		status.CompletedAt = &now
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(ops, phase)
	return ctrl.Result{}, r.releaseLock(ctx, ops)
}

// countTowardRealignment records that one more volume has moved, which is what
// the rebalancer's periodic loop reads to decide that a control-plane data
// realignment is owed.
//
// It is counted here rather than on every terminal phase, because only a move
// that landed leaves the cluster's data laid out against the placement it had
// before. An aborted or failed operation left the volume where it was.
//
// Best effort: a realignment that is late is not a realignment that is lost,
// since the next move to finish increments again and a realignment is
// idempotent. Failing the operation over it would be reporting a move that
// worked as one that did not.
func (r *PersistentVolumeOpsReconciler) countTowardRealignment(
	ctx context.Context, subject *subject,
) {
	name, err := vmigration.RecordVolumeMoved(
		ctx, r.Client, subject.namespace(), subject.clusterUUID)
	switch {
	case err != nil:
		logf.FromContext(ctx).Error(err, "the volume move could not be counted for the realignment",
			"cluster", subject.clusterUUID)
	case name == "":
		logf.FromContext(ctx).Info("no StorageCluster reports this volume's cluster, "+
			"so the move was not counted for the realignment", "cluster", subject.clusterUUID)
	}
}

// hold reports an operation that is admitted, holds nothing, and is waiting.
// Pending is both where an operation starts and where it waits, and
// status.deferredSince is what says since when — in status rather than in
// memory, because the operator may restart and an observer needs to see it.
func (r *PersistentVolumeOpsReconciler) hold(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		if status.Phase == "" {
			status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhasePending
		}
		if status.DeferredSince == nil {
			now := metav1.Now()
			status.DeferredSince = &now
		}
		status.Message = message
	})
}

// note replaces status.message without moving anything else. It is one sentence
// about where the operation is, replaced as it moves, and never a log.
func (r *PersistentVolumeOpsReconciler) note(
	ctx context.Context, ops *simplyblockv1alpha2.PersistentVolumeOps, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Message = message
	})
}

// recordStep persists the step the operation is about to be in, with the
// instant it expires. Both travel together, because a step persisted without
// its deadline restores as a step that can never time out.
func (r *PersistentVolumeOpsReconciler) recordStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	next step,
	deadline *metav1.Time,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.PersistentVolumeOpsStatus) {
		status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
		status.Step = statemachine.KubeSnapshot{State: string(next), Deadline: deadline}
	})
}

// writeStatus applies the mutation and patches only when something changed.
//
// Retried rather than swallowed on a conflict. A caller that read nil would
// take the write for done, and finish does: it releases the volume's lock
// straight afterward, so a dropped terminal status would free the volume for
// the next migration while this operation still reported Running.
func (r *PersistentVolumeOpsReconciler) writeStatus(
	ctx context.Context,
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	mutate func(*simplyblockv1alpha2.PersistentVolumeOpsStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.PersistentVolumeOps
		if err := r.Get(ctx, types.NamespacedName{Name: ops.Name}, &fresh); err != nil {
			return err
		}

		desired := *fresh.Status.DeepCopy()
		mutate(&desired)
		desired.ObservedGeneration = fresh.Generation

		if reflect.DeepEqual(fresh.Status, desired) {
			// Still published to the caller, which reads the object it passed
			// in on the next line of its own logic.
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

func (r *PersistentVolumeOpsReconciler) event(
	object client.Object, eventType, reason, format string, args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(object, nil, eventType, reason, reason, format, args...)
}

// terminal reports a phase the operation can never leave.
func terminal(phase simplyblockv1alpha2.PersistentVolumeOpsPhase) bool {
	switch phase {
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed,
		simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted:
		return true
	default:
		return false
	}
}

// memberCount is how many volumes the migrated subsystem holds, which is what
// the copy's deadline scales by. It is zero until the migration's creation
// reports one, and the graph turns that into the base bound.
func memberCount(ops *simplyblockv1alpha2.PersistentVolumeOps) int32 {
	if ops.Status.Migration == nil || ops.Status.Migration.MemberCount == nil {
		return 0
	}
	return *ops.Status.Migration.MemberCount
}

// terminalStepError is a step failure that retrying cannot fix: a volume this
// operator cannot address, a target node that is not this volume's, a control
// plane that refused the migration outright. It is a distinct type so that the
// reconcile loop can tell it from a control plane that is briefly unreachable,
// which is the same shape of error and the opposite response.
type terminalStepError struct{ reason string }

func (e *terminalStepError) Error() string { return e.reason }

func fatalf(format string, args ...any) error {
	return &terminalStepError{reason: fmt.Sprintf(format, args...)}
}

// errVolumeGone is the volume having been deleted or begun deleting, which ends
// the operation without it having gone wrong.
var errVolumeGone = errors.New("the PersistentVolume this operation acts on is gone")

// volumeLockChanged narrows the volume watch to what this controller reacts to:
// the lock changing hands, and the volume being deleted. A PersistentVolume is
// otherwise updated often enough — capacity, phase, claim binding — that
// watching every change would reconcile every operation in the cluster for
// reasons none of them care about.
type volumeLockChanged struct{}

func (volumeLockChanged) Create(event.TypedCreateEvent[client.Object]) bool { return false }

func (volumeLockChanged) Delete(event.TypedDeleteEvent[client.Object]) bool { return true }

func (volumeLockChanged) Generic(event.TypedGenericEvent[client.Object]) bool { return false }

func (volumeLockChanged) Update(e event.TypedUpdateEvent[client.Object]) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}
	was := e.ObjectOld.GetAnnotations()[simplyblockv1alpha2.PersistentVolumeOpsLock]
	is := e.ObjectNew.GetAnnotations()[simplyblockv1alpha2.PersistentVolumeOpsLock]
	if was != is {
		return true
	}
	return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
}

// ensure the predicate satisfies the interface it is passed as.
var _ predicate.TypedPredicate[client.Object] = volumeLockChanged{}
