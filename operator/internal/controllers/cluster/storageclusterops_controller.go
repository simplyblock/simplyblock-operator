// The StorageClusterOps reconciler: it drives one operation against one
// StorageCluster to a terminal phase and leaves the object behind as the record
// of what was done.
//
// Two machines run, not one. The outer phase — Pending, Running, and the three
// terminal values — is identical for every action, so folding it into each
// action's graph would copy that spine seven times and a later fix would land
// in one copy (design-crd-model.md §3.1). The inner one is the action's steps,
// declared in graphs.go.
//
// Nothing here blocks. One reconcile advances at most one step: it asks whether
// the current step has finished, and either requeues or writes the next step
// down and enters it. A step that has not finished is waiting on something
// outside this process, and waiting for it inline would hold a worker for as
// long as a cluster takes to come back up.
//
// The persisted position is the write-ahead record, and no flag sits beside it
// (§6.2). A step is written before the side effect that step performs, so a
// process dying between the two restarts into a state saying the call may
// already have landed. That is safe rather than merely tolerated: every step's
// completion condition is a predicate over current state, and every call is
// skipped when its target is already at or past the state that call would
// produce, so a cluster already shut down receives no second shutdown.

package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// OpsFinalizer is what guarantees the cluster's lock is released even when
	// the object is deleted mid-flight. Without it a `kubectl delete` on a
	// running rolling restart would leave the cluster locked by an object that
	// no longer exists, and nothing would ever unlock it (§8).
	OpsFinalizer = "storage.simplyblock.io/storageclusterops-finalizer"

	// opsRetry is how long an operation waits before looking again at
	// something it cannot hurry: a lock another operation holds, or a step
	// waiting on the control plane. A queued operation is normally woken by
	// its cluster rather than by this, and this is the backstop for when that
	// event is missed (design-storagecluster.md §6.1).
	opsRetry = 10 * time.Second

	// opsContended is how long a pass waits when its lock patch was refused
	// rather than when it found the lock held. The two are different
	// situations: a lock somebody visibly holds is released by work that has
	// to finish first, while a 409 means the object moved between this pass's
	// read and its write and who holds it now is one read away.
	//
	// It is shorter than opsRetry for that reason, and it is not zero. An
	// immediate requeue against an object two reconcilers are writing is a
	// spin: it burns a pass to re-read a value that has not settled, and it
	// does so fastest exactly when contention is highest.
	opsContended = 5 * time.Second

	// opsAdvance is how long a pass that moved the operation forward waits
	// before the next one. It is short because there is nothing to wait for:
	// the status write this pass made is itself a change the controller
	// watches, so this is the backstop for the event rather than the path the
	// next step normally arrives on.
	opsAdvance = time.Second

	// clusterRefField is the index a cluster event is mapped back through. It
	// is what makes a released lock wake the queue immediately rather than
	// after a requeue interval.
	clusterRefField = "spec.clusterRef"
)

// StorageClusterOpsReconciler reconciles a StorageClusterOps.
type StorageClusterOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	API      ControlPlane

	// The three stream caches every completion condition in this package is
	// evaluated against (§4.4). Each is optional: a deployment without the
	// control-plane informer, and every unit test that does not script one,
	// falls back to reading the control plane directly.
	//
	// They are the same caches the StorageCluster reconciler reads, and that
	// is the point of a cache rather than a second stream: the cluster's
	// status and the operation's completion condition are one reading, so two
	// controllers asking the same question get the same answer.
	Clusters ClusterCache
	Nodes    NodeCache
	Tasks    TaskCache
}

// NodeCache is the part of the storage-node subscription this package reads.
//
// It is declared here rather than imported from the subscription because an
// interface belongs to its consumer, and because the only other consumer —
// the StorageNode reconciler — lives in a package that now imports this one,
// so sharing one declaration would be a cycle.
type NodeCache interface {
	// List returns every node of a cluster, which is exactly what the walk
	// evaluates its predicates against.
	List(scope cpinformer.Scope) []subscriptions.NodeDTO

	// Synced reports whether the cluster's initial snapshot has been applied.
	// An empty unsynced cache and a cluster with no nodes look identical, and
	// reading the first as the second would plan a walk over nothing and
	// report it a success.
	Synced(scope cpinformer.Scope) bool
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusterops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusterops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusterops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodesets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// SetupWithManager registers the controller and maps an event on a
// StorageCluster back to every operation targeting it.
//
// That mapping is what makes the queue move. An operation waiting on a lock has
// nothing of its own to react to, so a controller watching only its own kind
// would leave every queued operation waiting out a requeue interval after the
// lock frees (design-crd-model.md §3.2).
func (r *StorageClusterOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&simplyblockv1alpha2.StorageClusterOps{}, clusterRefField,
		func(object client.Object) []string {
			ops, ok := object.(*simplyblockv1alpha2.StorageClusterOps)
			if !ok {
				return nil
			}
			return []string{ops.Spec.ClusterRef}
		})
	if err != nil {
		return fmt.Errorf("index cluster operations by their target: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageClusterOps{}).
		Named("storageclusterops").
		Watches(&simplyblockv1alpha2.StorageCluster{},
			handler.EnqueueRequestsFromMapFunc(r.operationsOn)).
		Complete(r)
}

// operationsOn enqueues every operation naming this cluster that has not
// finished. A terminal one has nothing to react to.
func (r *StorageClusterOpsReconciler) operationsOn(
	ctx context.Context, cluster client.Object,
) []reconcile.Request {
	var operations simplyblockv1alpha2.StorageClusterOpsList
	err := r.List(ctx, &operations,
		client.InNamespace(cluster.GetNamespace()),
		client.MatchingFields{clusterRefField: cluster.GetName()})
	if err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range operations.Items {
		if terminalOps(operations.Items[i].Status.Phase) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&operations.Items[i]),
		})
	}
	return requests
}

func (r *StorageClusterOpsReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var ops simplyblockv1alpha2.StorageClusterOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ops.DeletionTimestamp.IsZero() {
		return r.teardown(ctx, &ops)
	}

	if !controllerutil.ContainsFinalizer(&ops, OpsFinalizer) {
		controllerutil.AddFinalizer(&ops, OpsFinalizer)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	// A terminal operation is a record, and a record does nothing. Releasing
	// the lock here as well as on the transition is what covers the pass that
	// crashed between persisting the phase and clearing activeOpsRef, which
	// would otherwise leave the cluster locked by a finished operation forever.
	if terminalOps(ops.Status.Phase) {
		return ctrl.Result{}, r.releaseLock(ctx, &ops)
	}

	outcome, err := r.acquireLock(ctx, &ops)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch outcome {
	case lockHeld:
		return ctrl.Result{RequeueAfter: opsRetry}, nil
	case lockContended:
		return ctrl.Result{RequeueAfter: opsContended}, nil
	}

	return r.advance(ctx, &ops)
}

// advance runs the action's machine forward by at most one step.
func (r *StorageClusterOpsReconciler) advance(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) (ctrl.Result, error) {
	// Every control-plane call this step and everything downstream of it
	// makes (perform, advanceWalk, and everything under them) authenticates
	// as this operation's own cluster when its secret is known, rather than
	// as this operator's Kubernetes identity -- the only way to reach a
	// control plane a different Kubernetes cluster runs (a
	// ControlPlane.spec.source.managed one), since a Kubernetes TokenReview
	// can never cross a cluster boundary.
	ctx = r.authenticatedContext(ctx, ops)

	graph := action(ops.Spec.Action)
	machine, err := graphs().FromSnapshot(ctx, graph,
		statemachine.FromKube[step](ops.Status.Step))
	if err != nil {
		// An unrecognized step or action is a downgrade, a hand-edited object,
		// or a rename that shipped without a conversion, and none of them
		// resolve by reconciling again. The operation is terminal with the
		// reason in status.message, which leaves a record saying so.
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseFailed,
			fmt.Sprintf("the operation cannot be resumed: %v", err))
	}
	defer machine.Close()

	// A machine is born already in its initial state, so that state's entry
	// hook never runs and no deadline is set for it. Setting one on the first
	// pass is what stops the first step being the one step that cannot time
	// out.
	if ops.Status.Step.State == "" {
		return r.enterInitialStep(ctx, ops, machine)
	}

	current := machine.CurrentState()

	if ops.Spec.Abort {
		return r.unwind(ctx, ops, machine, current)
	}

	if machine.TimeoutReached() {
		operationStepDeadlineExceededTotal.
			WithLabelValues(ops.Spec.ClusterRef, string(ops.Spec.Action), string(current)).Inc()
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
			StepDeadlineExceeded, StepDeadlineExceeded,
			"Step %s outlived its deadline", current)
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseFailed,
			fmt.Sprintf("step %s outlived its deadline", current))
	}

	// A walk whose index has reached the end of its list is finished, whatever
	// step the machine is on. That covers the cluster with no nodes, whose
	// plan is empty from the start, and the operation resumed after its last
	// node advanced the index but crashed before the phase was written.
	if ops.Spec.Action == simplyblockv1alpha2.StorageClusterOpsActionRollingRestart &&
		walkFinished(ops) {
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded,
			r.successMessage(ops))
	}

	done, err := r.perform(ctx, ops, current)
	if err != nil {
		var fatal *terminalStepError
		if errors.As(err, &fatal) {
			return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseFailed, fatal.Error())
		}
		logf.FromContext(ctx).Error(err, "the step could not be advanced",
			"operation", ops.Name, "step", current)
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
	}
	if !done {
		return r.waitOn(machine), r.note(ctx, ops, r.waitingMessage(ops, current))
	}

	r.observeStep(ops, current)

	if machine.IsTerminal() {
		// A rolling restart's terminal step ends one node rather than the
		// operation. Every other action is finished when its graph is.
		if ops.Spec.Action == simplyblockv1alpha2.StorageClusterOpsActionRollingRestart {
			return r.advanceWalk(ctx, ops, machine)
		}
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded,
			r.successMessage(ops))
	}

	next, err := r.nextStep(ops, machine)
	if err != nil {
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseFailed, err.Error())
	}
	return r.enterStep(ctx, ops, machine, next)
}

// enterStep moves the machine into the next step and writes the step and the
// deadline its entry hook armed in one patch.
//
// The two go together because a step with no deadline is a step nothing can
// ever time out: TimeoutReached reads the stored deadline, so a crash between
// a patch carrying the state and a later one carrying the deadline would
// restore an operation that retries on the fallback interval for good and
// never reports the failure its budget exists to produce.
//
// Writing after the transition rather than before it costs nothing, because
// every OnEnter in graphs.go returns a duration and performs nothing. The
// side effect of a step is performed on the pass that follows, against the
// step this patch persisted, which is where the write-ahead record is needed
// and what it records.
func (r *StorageClusterOpsReconciler) enterStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	machine *statemachine.Machine[step],
	next step,
) (ctrl.Result, error) {
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	snapshot := statemachine.ToKube(machine.Snapshot())
	return ctrl.Result{RequeueAfter: opsAdvance}, r.recordStep(ctx, ops, next, snapshot.Deadline)
}

// enterInitialStep sets the first step's deadline and moves the operation to
// Running.
func (r *StorageClusterOpsReconciler) enterInitialStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	machine *statemachine.Machine[step],
) (ctrl.Result, error) {
	budget, ok := initialDeadlines[action(ops.Spec.Action)]
	if !ok {
		budget = requestingDeadline
	}
	deadline := metav1.NewTime(time.Now().Add(budget))
	if err := r.recordStep(ctx, ops, machine.CurrentState(), &deadline); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
		OperationStarted, OperationStarted,
		"The operation acquired the lock on cluster %s and started", ops.Spec.ClusterRef)
	return ctrl.Result{RequeueAfter: opsAdvance}, nil
}

// nextStep is the step that follows the current one. Most graphs here are a
// line, so the first edge is the only edge; ShuttingDownNode is the one branch,
// and spec.rollingRestart.refreshSNodeAPI decides it.
func (r *StorageClusterOpsReconciler) nextStep(
	ops *simplyblockv1alpha2.StorageClusterOps, machine *statemachine.Machine[step],
) (step, error) {
	current := machine.CurrentState()
	if current == stepShuttingDownNode {
		if refreshesPod(ops) {
			return stepRefreshingPod, nil
		}
		return stepRestartingNode, nil
	}
	for next := range machine.AllowedTransitions() {
		return next, nil
	}
	return current, fmt.Errorf("step %s declares no successor and is not terminal", current)
}

// unwind honors spec.abort where the graph allows it, and reports an abort that
// arrived too late rather than half-undoing the work.
//
// The refusal is the point. A step with no abort edge has already asked the
// control plane for something it is part-way through, and stopping there would
// leave nothing driving the cluster back to a state somebody can reason about.
func (r *StorageClusterOpsReconciler) unwind(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	machine *statemachine.Machine[step],
	current step,
) (ctrl.Result, error) {
	// The machine is asked rather than a table beside it, and it is asked rather
	// than the graphs, because it was built for this operation's action: a step
	// two actions share can be abortable in one of them.
	if !machine.CanAbort() {
		// Not a failure of the operation: it carries on. What the user asked
		// for cannot be done, and saying so is the whole of the response.
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, fmt.Sprintf(
			"the abort arrived at step %s, which the control plane is part-way through "+
				"and cannot be stopped; the operation is running on", current))
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
		OperationAborted, OperationAborted,
		"The operation was aborted at step %s", current)
	return r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseAborted,
		fmt.Sprintf("aborted at step %s", current))
}

// waitOn requeues for whatever is left of the current step's deadline, so that
// a step with a long budget is looked at when it expires rather than on a fixed
// interval, and a step with a short one is not left waiting past it.
func (r *StorageClusterOpsReconciler) waitOn(machine *statemachine.Machine[step]) ctrl.Result {
	if remaining, bounded := machine.RequeueAfter(); bounded && remaining < opsRetry {
		return ctrl.Result{RequeueAfter: remaining}
	}
	return ctrl.Result{RequeueAfter: opsRetry}
}

// finish writes a terminal phase, releases the cluster's lock, and records what
// the operation cost.
func (r *StorageClusterOpsReconciler) finish(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	phase simplyblockv1alpha2.StorageClusterOpsPhase,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.Phase = phase
		status.Message = message
		status.CompletedAt = &now
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	switch phase {
	case simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded:
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
			OperationSucceeded, OperationSucceeded, "%s", message)
	case simplyblockv1alpha2.StorageClusterOpsPhaseFailed:
		r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
			OperationFailed, OperationFailed, "%s", message)
	}

	r.observeOperation(ops, phase)
	return ctrl.Result{}, r.releaseLock(ctx, ops)
}

// observeOperation records the operation's outcome and how long it ran.
func (r *StorageClusterOpsReconciler) observeOperation(
	ops *simplyblockv1alpha2.StorageClusterOps,
	phase simplyblockv1alpha2.StorageClusterOpsPhase,
) {
	cluster, action, result := ops.Spec.ClusterRef, string(ops.Spec.Action), resultOf(phase)
	operationsTotal.WithLabelValues(cluster, action, result).Inc()
	if started := ops.Status.StartedAt; started != nil {
		operationDurationSeconds.WithLabelValues(cluster, action, result).
			Observe(time.Since(started.Time).Seconds())
	}
	rollingRestartNodeIndex.DeleteLabelValues(cluster)
	rollingRestartNodeCount.DeleteLabelValues(cluster)
}

// observeStep records how long one step took. The start is the step's entry,
// which is its deadline minus the budget the graph gives it, so no second
// timestamp has to be persisted for a measurement.
func (r *StorageClusterOpsReconciler) observeStep(
	ops *simplyblockv1alpha2.StorageClusterOps, current step,
) {
	deadline, bounded := ops.Status.Step.KubeDeadline()
	if !bounded {
		return
	}
	budget, ok := stepBudgets[current]
	if !ok {
		return
	}
	elapsed := time.Since(deadline.Add(-budget)).Seconds()
	if elapsed < 0 {
		return
	}
	operationStepDurationSeconds.
		WithLabelValues(ops.Spec.ClusterRef, string(ops.Spec.Action), string(current)).
		Observe(elapsed)
}

// stepBudgets is what each step's deadline was set from, which is the other
// half of the arithmetic in observeStep. It is derived from the graph's
// deadlines rather than restated, so a budget changed in one place moves both.
var stepBudgets = map[step]time.Duration{
	stepRequesting:       requestingDeadline,
	stepAwaiting:         awaitingDeadline,
	stepShuttingDown:     awaitingDeadline,
	stepStarting:         awaitingDeadline,
	stepCheckingPeers:    checkingPeersDeadline,
	stepShuttingDownNode: nodeShutdownDeadline,
	stepRefreshingPod:    podRefreshDeadline,
	stepAwaitingPod:      podRefreshDeadline,
	stepRestartingNode:   nodeRestartDeadline,
	stepRebalancing:      rebalancingDeadline,
}

// resultOf is the metric label for a terminal phase, lowercased because a label
// value is not an API enum.
func resultOf(phase simplyblockv1alpha2.StorageClusterOpsPhase) string {
	switch phase {
	case simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded:
		return "succeeded"
	case simplyblockv1alpha2.StorageClusterOpsPhaseAborted:
		return "aborted"
	default:
		return "failed"
	}
}

// teardown releases the lock and lets the object go. It is the third release
// path and the one that matters most, because `kubectl delete` on a running
// operation would otherwise leave the cluster locked by an object that no
// longer exists.
func (r *StorageClusterOpsReconciler) teardown(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ops, OpsFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.releaseLock(ctx, ops); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(ops, OpsFinalizer)
	return ctrl.Result{}, r.Update(ctx, ops)
}

// acquireLock takes the cluster's status.activeOpsRef, and reports whether this
// operation now holds it.
//
// Acquisition is an optimistic-lock patch rather than a plain one, which is
// what makes the read-then-write safe: two operations can both read an empty
// field and both conclude the lock is free, and the patch succeeds for exactly
// one of them at a given resourceVersion and returns 409 to the rest.
//
// The outcome is typed rather than a bool, because "not acquired" is two
// situations with different waits (§6.1) and a bool collapses them.
func (r *StorageClusterOpsReconciler) acquireLock(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) (lockOutcome, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Name: ops.Spec.ClusterRef, Namespace: ops.Namespace}
	err := r.Get(ctx, key, &cluster)
	if apierrors.IsNotFound(err) {
		_, err := r.finish(ctx, ops, simplyblockv1alpha2.StorageClusterOpsPhaseFailed,
			fmt.Sprintf("StorageCluster %s does not exist", ops.Spec.ClusterRef))
		return lockHeld, err
	}
	if err != nil {
		return lockHeld, err
	}

	if held := cluster.Status.ActiveOpsRef; held != "" && held != ops.Name {
		r.Recorder.Eventf(ops, nil, corev1.EventTypeNormal,
			OperationQueued, OperationQueued,
			"Cluster %s is held by operation %s; this one is waiting", cluster.Name, held)
		return lockHeld, r.hold(ctx, ops, fmt.Sprintf(
			"waiting for operation %s to release cluster %s", held, cluster.Name))
	}

	if cluster.Status.ActiveOpsRef != ops.Name {
		patch := client.MergeFromWithOptions(cluster.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		cluster.Status.ActiveOpsRef = ops.Name
		if err := r.Status().Patch(ctx, &cluster, patch); err != nil {
			if apierrors.IsConflict(err) {
				// Somebody else moved the object between the read and the
				// write. Whether that was another operation taking the lock is
				// decided by reading it again rather than guessed at here.
				return lockContended, nil
			}
			return lockHeld, fmt.Errorf("acquire the lock on cluster %s: %w", cluster.Name, err)
		}
		operationActiveState.WithLabelValues(cluster.Name).Set(1)
	}

	if ops.Status.Phase == "" || ops.Status.Phase == simplyblockv1alpha2.StorageClusterOpsPhasePending {
		now := metav1.Now()
		// How long the operation waited behind another one's lock. The object's
		// creation is the start, because Pending is where an operation both
		// begins and waits.
		operationLockWaitSeconds.WithLabelValues(cluster.Name, string(ops.Spec.Action)).
			Observe(now.Sub(ops.CreationTimestamp.Time).Seconds())
		err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
			status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
			status.StartedAt = &now
			status.Message = "The operation holds the cluster and is running"
		})
		if err != nil {
			return lockHeld, err
		}
	}
	return lockAcquired, nil
}

// lockOutcome is what one attempt at a cluster's lock produced. The two
// unsuccessful values are separate because they are waited on differently
// (§6.1): a lock somebody holds frees when their work finishes, and a refused
// patch resolves on the next read.
type lockOutcome int

const (
	// lockAcquired: this operation holds the cluster.
	lockAcquired lockOutcome = iota
	// lockHeld: another operation holds it, or the attempt could not be made.
	lockHeld
	// lockContended: the optimistic-lock patch was refused.
	lockContended
)

// releaseLock clears the cluster's status.activeOpsRef, but only while it still
// names this operation.
//
// The ownership check is what makes a late release safe. A pass that started
// before the lock changed hands would otherwise clear a lock somebody else now
// holds, which is worse than not releasing at all: two operations would then be
// running against one cluster with neither of them knowing.
func (r *StorageClusterOpsReconciler) releaseLock(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) error {
	key := types.NamespacedName{Name: ops.Spec.ClusterRef, Namespace: ops.Namespace}

	// The conflict is retried here rather than reported, and the compare-and-swap
	// is what makes that safe rather than a shortcut: every attempt re-reads the
	// cluster and checks the lock is still this operation's, so a release that
	// lost a race to somebody taking the lock finds that on the next read and
	// clears nothing. Swallowing the conflict without the re-read is the thing
	// that would be wrong — it would let the caller reach a terminal phase and
	// drop its finalizer while the cluster stayed locked by an object that no
	// longer exists.
	//
	// Reporting it failed the reconcile instead, which preserved the same
	// property by a longer route: a stack trace for an operation that had just
	// succeeded, over a conflict with the cluster's own reconciler writing the
	// status it writes on every pass.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cluster simplyblockv1alpha2.StorageCluster
		if err := r.Get(ctx, key, &cluster); err != nil {
			return err
		}
		if cluster.Status.ActiveOpsRef != ops.Name {
			// Never held, already released, or taken by somebody else between
			// two attempts. None of them is this operation's to undo.
			return nil
		}

		patch := client.MergeFromWithOptions(cluster.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		cluster.Status.ActiveOpsRef = ""
		return r.Status().Patch(ctx, &cluster, patch)
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the lock on cluster %s: %w", ops.Spec.ClusterRef, err)
	}

	operationActiveState.WithLabelValues(ops.Spec.ClusterRef).Set(0)
	return nil
}

// hold reports an operation that is admitted, holds nothing, and is waiting.
// Pending is both where an operation starts and where it waits, and the
// OperationQueued event is the only thing that separates the two.
func (r *StorageClusterOpsReconciler) hold(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		if status.Phase == "" {
			status.Phase = simplyblockv1alpha2.StorageClusterOpsPhasePending
		}
		status.Message = message
	})
}

// note replaces status.message without moving anything else. It is one sentence
// about where the operation is, replaced as it moves, and never a log.
func (r *StorageClusterOpsReconciler) note(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.Message = message
	})
}

// recordStep persists the step the operation is about to be in, with the
// instant it expires. Both travel together, because a step persisted without
// its deadline restores as a step that can never time out.
func (r *StorageClusterOpsReconciler) recordStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	next step,
	deadline *metav1.Time,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageClusterOpsStatus) {
		status.Phase = simplyblockv1alpha2.StorageClusterOpsPhaseRunning
		status.Step = statemachine.KubeSnapshot{State: string(next), Deadline: deadline}
	})
}

// writeStatus applies the mutation and patches only when something changed.
//
// observedGeneration is written here rather than by each caller, and on this
// kind its second advance is precisely the signal that spec.abort has been
// observed: a user who sets it and sees an unchanged status cannot otherwise
// tell a controller that has not looked from one that looked and declined.
func (r *StorageClusterOpsReconciler) writeStatus(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageClusterOps,
	mutate func(*simplyblockv1alpha2.StorageClusterOpsStatus),
) error {
	// Retried rather than swallowed on a conflict. A caller that read nil
	// would take the write for done, and finish does: it releases the
	// cluster's lock straight afterward, so a dropped terminal status would
	// free the cluster for the next operation while this one still reported
	// Running.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.StorageClusterOps
		if err := r.Get(ctx, client.ObjectKeyFromObject(ops), &fresh); err != nil {
			return err
		}

		desired := *fresh.Status.DeepCopy()
		mutate(&desired)
		desired.ObservedGeneration = fresh.Generation

		if equalOpsStatus(fresh.Status, desired) {
			// Still published to the caller, which reads the object it passed
			// in on the next line of its own logic.
			ops.Status = desired
			ops.ResourceVersion = fresh.ResourceVersion
			return nil
		}

		patch := client.MergeFromWithOptions(fresh.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		fresh.Status = desired
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		ops.Status = fresh.Status
		ops.ResourceVersion = fresh.ResourceVersion
		return nil
	})
}

// terminalOps reports a phase the operation can never leave.
func terminalOps(phase simplyblockv1alpha2.StorageClusterOpsPhase) bool {
	switch phase {
	case simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageClusterOpsPhaseFailed,
		simplyblockv1alpha2.StorageClusterOpsPhaseAborted:
		return true
	default:
		return false
	}
}

// terminalStepError is a step failure that retrying cannot fix: an action the
// control plane refused outright, a task that does not exist, a cluster with no
// UUID. It is a distinct type so that the reconcile loop can tell it from a
// control plane that is briefly unreachable, which is the same shape of error
// and the opposite response.
type terminalStepError struct{ reason string }

func (e *terminalStepError) Error() string { return e.reason }

func fatalf(format string, args ...any) error {
	return &terminalStepError{reason: fmt.Sprintf(format, args...)}
}

// refreshesPod reports whether the rolling restart replaces each node's
// storage-node pod between its shutdown and its restart.
func refreshesPod(ops *simplyblockv1alpha2.StorageClusterOps) bool {
	return ops.Spec.RollingRestart != nil && ops.Spec.RollingRestart.RefreshSNodeAPI
}

// clusterUUIDFor resolves the operation's target to the control plane's
// identifier. A cluster with no UUID has never been created, so there is
// nothing for any action to act on and retrying will not produce one from here.
func (r *StorageClusterOpsReconciler) clusterUUIDFor(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) (string, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Name: ops.Spec.ClusterRef, Namespace: ops.Namespace}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return "", err
	}
	if cluster.Status.UUID == "" {
		return "", fatalf("cluster %s has not been created in the control plane yet",
			ops.Spec.ClusterRef)
	}
	return cluster.Status.UUID, nil
}

// equalOpsStatus compares two statuses for the purpose of deciding whether to
// write. It is spelled out rather than reflect.DeepEqual because the status
// carries pointers to timestamps, and two equal instants behind two pointers
// are not deeply equal.
func equalOpsStatus(a, b simplyblockv1alpha2.StorageClusterOpsStatus) bool {
	if a.Phase != b.Phase || a.Message != b.Message ||
		a.ObservedGeneration != b.ObservedGeneration ||
		a.Step.State != b.Step.State ||
		!equalTime(a.Step.Deadline, b.Step.Deadline) ||
		!equalTime(a.StartedAt, b.StartedAt) ||
		!equalTime(a.CompletedAt, b.CompletedAt) {
		return false
	}
	if (a.RollingRestart == nil) != (b.RollingRestart == nil) {
		return false
	}
	if a.RollingRestart == nil {
		return true
	}
	if a.RollingRestart.NodeIndex != b.RollingRestart.NodeIndex ||
		len(a.RollingRestart.Nodes) != len(b.RollingRestart.Nodes) {
		return false
	}
	for i := range a.RollingRestart.Nodes {
		if a.RollingRestart.Nodes[i] != b.RollingRestart.Nodes[i] {
			return false
		}
	}
	return true
}

func equalTime(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

// successMessage is what a finished operation says it did. The rolling restart
// has its own, because "all 12 nodes restarted" is the only number a reader
// wants from it.
func (r *StorageClusterOpsReconciler) successMessage(
	ops *simplyblockv1alpha2.StorageClusterOps,
) string {
	if ops.Spec.Action == simplyblockv1alpha2.StorageClusterOpsActionRollingRestart {
		return fmt.Sprintf("all %d nodes restarted", len(walkOf(ops).Nodes))
	}
	return fmt.Sprintf("the %s completed on cluster %s",
		ops.Spec.Action, ops.Spec.ClusterRef)
}

// waitingMessage is what an unfinished step says. The rolling restart's says
// which node it is on as well as which step, because the step alone does not
// locate a walk.
func (r *StorageClusterOpsReconciler) waitingMessage(
	ops *simplyblockv1alpha2.StorageClusterOps, current step,
) string {
	if ops.Spec.Action != simplyblockv1alpha2.StorageClusterOpsActionRollingRestart {
		return fmt.Sprintf("waiting on %s", current)
	}
	return walkMessage(ops, string(current))
}

// clusterActive reports the completion condition four of the seven actions
// share, read against the control plane's current answer rather than against a
// transition it might have missed.
func (r *StorageClusterOpsReconciler) clusterActive(
	ctx context.Context, clusterID string,
) (bool, error) {
	reading, err := r.clusterReading(ctx, clusterID)
	if err != nil {
		return false, err
	}
	return reading.Status == utils.ClusterStatusActive, nil
}

// clusterReading is what the control plane currently says about the cluster,
// from the stream's cache once it has delivered its snapshot and from the
// control plane until then.
//
// An operation asks this on every pass of every step, so a shutdown that takes
// twenty minutes is eighty reads the informer is already holding. The gate is
// the root snapshot rather than a preference, because a cluster missing from
// an unsynced cache and one the control plane has forgotten look identical,
// and reading the first as the second would report a shutdown complete while
// the cluster is still up.
func (r *StorageClusterOpsReconciler) clusterReading(
	ctx context.Context, clusterID string,
) (subscriptions.ClusterDTO, error) {
	if r.Clusters != nil && r.Clusters.SyncedRoot() {
		if dto, ok := r.Clusters.Lookup(clusterID); ok {
			return dto, nil
		}
		return subscriptions.ClusterDTO{},
			fmt.Errorf("the control plane no longer reports cluster %s", clusterID)
	}

	response, err := r.API.Cluster(ctx, clusterID)
	if err != nil {
		return subscriptions.ClusterDTO{}, fmt.Errorf("read cluster %s: %w", clusterID, err)
	}
	return subscriptions.ClusterDTO{
		ID:                response.UUID,
		NQN:               response.NQN,
		Status:            response.Status,
		Rebalancing:       response.Rebalancing,
		NDCS:              response.NDCS,
		NPCS:              response.NPCS,
		MaxFaultTolerance: response.MaxFaultTolerance,
	}, nil
}

// clusterSecret reads the secret StorageClusterReconciler.persist wrote for
// this operation's cluster, keyed by the StorageCluster's Kubernetes name (not
// its backend UUID, which this reconciler is not always given yet at the point
// it needs the credential). It reports the empty string when there is none.
func (r *StorageClusterOpsReconciler) clusterSecret(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) (string, error) {
	var secret corev1.Secret
	key := types.NamespacedName{
		Name:      fmt.Sprintf("simplyblock-cluster-%s", ops.Spec.ClusterRef),
		Namespace: ops.Namespace,
	}
	if err := r.Get(ctx, key, &secret); err != nil {
		return "", err
	}
	return string(secret.Data["secret"]), nil
}

// authenticatedContext attaches this operation's cluster's own credential to
// ctx when one is known, so every control-plane call the operation makes
// authenticates as that cluster instead of as this operator's Kubernetes
// identity -- the only way to reach a control plane a different Kubernetes
// cluster runs (a ControlPlane.spec.source.managed one), since a Kubernetes
// TokenReview can never cross a cluster boundary. See StorageClusterReconciler's
// identically-named method.
func (r *StorageClusterOpsReconciler) authenticatedContext(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) context.Context {
	secret, err := r.clusterSecret(ctx, ops)
	if err != nil || secret == "" {
		return ctx
	}
	return webapi.WithBearerToken(ctx, secret)
}

// effectiveConcurrentRestarts is min(specVal, FTT), defaulting to 1 when the
// spec says nothing. Both inputs may be absent, because the fault tolerance
// comes from the control plane.
func effectiveConcurrentRestarts(specVal, ftt *int32) *int32 {
	effective := int32(1)
	if specVal != nil && *specVal > 0 {
		effective = *specVal
	}
	if ftt != nil && *ftt > 0 && *ftt < effective {
		effective = *ftt
	}
	return ptr.To(effective)
}
