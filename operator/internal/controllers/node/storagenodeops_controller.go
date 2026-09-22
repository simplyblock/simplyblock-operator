// The StorageNodeOps reconciler: it drives one operation against one StorageNode
// to a terminal phase and leaves the object behind as the record of what was
// done, to which node, with which parameters, and how it ended.
//
// Two machines run, not one. The outer phase — Pending, Running, and the three
// terminal values — is identical for every action, so folding it into each
// action's graph would copy that spine seven times and a later fix would land in
// one copy (design-crd-model.md §3.1). The inner one is the action's steps,
// declared in graphs.go.
//
// Nothing here blocks. One reconcile advances at most one step: it asks whether
// the current step has finished, and either requeues or writes the next step down
// and enters it. A step that has not finished is waiting on something outside this
// process, and waiting for it inline would hold a worker for as long as a drain
// takes.
//
// The persisted position is the write-ahead record, and no flag sits beside it
// (§7.2). A step is written before the side effect that step performs, so a
// process dying between the two restarts into a state saying the call may already
// have landed. That is safe rather than merely tolerated: every step's completion
// condition is a predicate over current state, and every call is skipped when its
// target is already at or past the state that call would produce, so a node
// already suspended receives no second suspend.
//
// design-storagenode.md §7 is the specification.

package node

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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

const (
	// OpsFinalizer is what guarantees the node's lock is released even when the
	// object is deleted mid-flight. Without it a `kubectl delete` on a running
	// drain would leave the node locked by an object that no longer exists, and
	// nothing would ever unlock it (§11).
	OpsFinalizer = "storage.simplyblock.io/storagenodeops-finalizer"

	// opsRetry is how long an operation waits before looking again at something
	// it cannot hurry: a lock another operation holds, a cluster that is not
	// ready, or a step waiting on the control plane. A queued operation is
	// normally woken by its node rather than by this, and this is the backstop
	// for when that event is missed.
	opsRetry = 15 * time.Second

	// opsAdvance is how long a pass that moved the operation forward waits
	// before the next one. It is short because there is nothing to wait for: the
	// status write this pass made is itself a change the controller watches, so
	// this is the backstop for the event rather than the path the next step
	// normally arrives on.
	opsAdvance = time.Second

	// nodeRefField is the index a node event is mapped back through. It is what
	// makes a released lock wake the queue immediately rather than after a
	// requeue interval.
	nodeRefField = "spec.nodeRef"
)

// StorageNodeOpsReconciler reconciles a StorageNodeOps.
type StorageNodeOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	API      ControlPlane

	// The two stream caches every completion condition in this package is
	// evaluated against (§4.4). Each is optional: a deployment without the
	// control-plane informer, and every unit test that does not script one, falls
	// back to reading the control plane directly.
	//
	// They are the same caches the StorageNode reconciler reads, and that is the
	// point of a cache rather than a second stream: the node's status and the
	// operation's completion condition are one reading, so two controllers asking
	// the same question get the same answer.
	Nodes    NodeCache
	Clusters ClusterCache

	// Mover raises a drain's fan-out as whichever kind this deployment runs. A
	// drain decides which volumes move where and has no business knowing which
	// kind carries them; unset means the kind this API group documents.
	Mover vmigration.Mover

	// Workload is the storage-plane side of a node: the worker labels, the
	// storage-node pod, its published DNS name, and the eviction budget a
	// maintenance window holds. Three of the seven actions touch it, and the
	// objects behind it belong to the StorageCluster (§5.1).
	Workload *Workload
}

// NodeCache is the part of the storage-node subscription this package reads.
type NodeCache interface {
	// Triggers is the reconcile-trigger stream; each event names a StorageNode.
	// Reading the cache without it makes the subscription a cache rather than a
	// push: a node's status is read from the stream instead of the control plane
	// and still waits out the requeue interval to be noticed.
	Triggers() <-chan event.GenericEvent

	// Lookup returns one node by its backend id, which is what every completion
	// condition in this package is a predicate over. The scope it comes back with
	// is the cluster the node was streamed under, which this package already knows
	// and does not read.
	Lookup(nodeID string) (cpinformer.Scope, subscriptions.NodeDTO, bool)

	// List returns every node of a cluster, which adoption matches against and
	// which the parallel-add and peer-selection predicates walk.
	List(scope cpinformer.Scope) []subscriptions.NodeDTO

	// Synced reports whether the cluster's initial snapshot has been applied. An
	// empty unsynced cache and a cluster with no nodes look identical, and
	// reading the first as the second would report a drain complete before it
	// started.
	Synced(scope cpinformer.Scope) bool
}

// ClusterCache is the part of the cluster subscription this package reads. The
// gate of §7.1 is the only thing that asks: whether the node's cluster is active
// and not mid-rebalance.
type ClusterCache interface {
	Lookup(clusterID string) (subscriptions.ClusterDTO, bool)
	SyncedRoot() bool
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodeops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// SetupWithManager registers the controller and maps an event on a StorageNode
// back to every operation targeting it.
//
// That mapping is what makes the queue move. An operation waiting on a lock has
// nothing of its own to react to, so a controller watching only its own kind
// would leave every queued operation waiting out a requeue interval after the
// lock frees (design-crd-model.md §3.2).
func (r *StorageNodeOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&simplyblockv1alpha2.StorageNodeOps{}, nodeRefField,
		func(object client.Object) []string {
			ops, ok := object.(*simplyblockv1alpha2.StorageNodeOps)
			if !ok {
				return nil
			}
			return []string{ops.Spec.NodeRef}
		})
	if err != nil {
		return fmt.Errorf("index node operations by their target: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.StorageNodeOps{}).
		Named("storagenodeops").
		Watches(&simplyblockv1alpha2.StorageNode{},
			handler.EnqueueRequestsFromMapFunc(r.operationsOn)).
		Complete(r)
}

// operationsOn enqueues every operation naming this node that has not finished.
// A terminal one has nothing to react to.
func (r *StorageNodeOpsReconciler) operationsOn(
	ctx context.Context, node client.Object,
) []reconcile.Request {
	var operations simplyblockv1alpha2.StorageNodeOpsList
	err := r.List(ctx, &operations,
		client.InNamespace(node.GetNamespace()),
		client.MatchingFields{nodeRefField: node.GetName()})
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

func (r *StorageNodeOpsReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var ops simplyblockv1alpha2.StorageNodeOps
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

	// A terminal operation is a record, and a record does nothing. Releasing the
	// lock here as well as on the transition is what covers the pass that crashed
	// between persisting the phase and clearing activeOpsRef, which would
	// otherwise leave the node locked by a finished operation forever.
	if terminalOps(ops.Status.Phase) {
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
func (r *StorageNodeOpsReconciler) advance(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (ctrl.Result, error) {
	// Every control-plane call this step and everything downstream of it
	// makes authenticates as this operation's target node's cluster when its
	// secret is known, rather than as this operator's own Kubernetes
	// identity -- the only way to reach a control plane a different
	// Kubernetes cluster runs (a ControlPlane.spec.source.managed one),
	// since a Kubernetes TokenReview can never cross a cluster boundary.
	secret, secretErr := clusterSecretForNode(ctx, r.Client, ops.Namespace, ops.Spec.NodeRef)
	ctx = authenticatedContext(ctx, secret, secretErr)

	machine, err := graphs().FromSnapshot(ctx, action(ops.Spec.Action),
		statemachine.FromKube[step](ops.Status.Step))
	if err != nil {
		// An unrecognized step or action is a downgrade, a hand-edited object, or
		// a rename that shipped without a conversion, and none of them resolve by
		// reconciling again. The operation is terminal with the reason in
		// status.message, which leaves a record saying so (§6.3).
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageNodeOpsPhaseFailed,
			fmt.Sprintf("the operation cannot be resumed: %v", err))
	}
	defer machine.Close()

	// A machine is born already in its initial state, so that state's entry hook
	// never runs and no deadline is set for it. Setting one on the first pass is
	// what stops the first step being the one step that cannot time out.
	if ops.Status.Step.State == "" {
		return r.enterInitialStep(ctx, ops, machine)
	}

	current := machine.CurrentState()

	if ops.Spec.Abort {
		return r.unwind(ctx, ops, machine, current)
	}

	// The cluster gate is not the same as the lock. A node operation runs inside
	// a cluster, and one whose cluster is mid-rebalance or not active will either
	// be rejected by the control plane or succeed into an inconsistent layout. It
	// holds rather than fails, and resumes when the cluster does (§7.1).
	if !skipsClusterGate(ops) {
		if ready, reason, err := r.clusterReady(ctx, ops); err != nil {
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
		} else if !ready {
			r.emit(ctx, ops, corev1.EventTypeWarning, ClusterNotReady, reason)
			return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, reason)
		}
	}

	if machine.TimeoutReached() {
		operationStepDeadlineExceededTotal.
			WithLabelValues(r.clusterLabel(ctx, ops), string(ops.Spec.Action), string(current)).Inc()
		r.emit(ctx, ops, corev1.EventTypeWarning, StepDeadlineExceeded,
			fmt.Sprintf("Step %s outlived its deadline", current))
		return r.fail(ctx, ops, current,
			fmt.Sprintf("step %s outlived its deadline", current))
	}

	done, err := r.perform(ctx, ops, current)
	if err != nil {
		var fatal *terminalStepError
		if errors.As(err, &fatal) {
			return r.fail(ctx, ops, current, fatal.Error())
		}
		var blocked *blockedStepError
		if errors.As(err, &blocked) {
			// A blocked step is correct behavior waiting on a human or on
			// another node coming back, and the event is what distinguishes it
			// from a stalled controller (§13.1). It is not a failure, so the
			// deadline keeps running and the operation keeps looking.
			r.emit(ctx, ops, corev1.EventTypeWarning, blocked.reason, blocked.message)
			return r.waitOn(machine), r.note(ctx, ops, blocked.message)
		}
		logf.FromContext(ctx).Error(err, "the step could not be advanced",
			"operation", ops.Name, "step", current)
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, err.Error())
	}
	if !done {
		return r.waitOn(machine), r.note(ctx, ops, r.waitingMessage(ops, current))
	}

	r.observeStep(ctx, ops, current)

	if machine.IsTerminal() {
		return r.finish(ctx, ops, simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded,
			r.successMessage(ops))
	}

	next, err := r.nextStep(machine)
	if err != nil {
		return r.fail(ctx, ops, current, err.Error())
	}
	return r.enterStep(ctx, ops, machine, next)
}

// enterStep moves the machine into the next step and writes the step and the
// deadline its entry hook armed in one patch.
//
// The two go together because a step with no deadline is a step nothing can ever
// time out: TimeoutReached reads the stored deadline, so a crash between a patch
// carrying the state and a later one carrying the deadline would restore an
// operation that retries on the fallback interval for good and never reports the
// failure its budget exists to produce.
//
// Writing after the transition rather than before it costs nothing, because every
// OnEnter in graphs.go returns a duration and performs nothing. The side effect of
// a step is performed on the pass that follows, against the step this patch
// persisted, which is where the write-ahead record is needed and what it records.
func (r *StorageNodeOpsReconciler) enterStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
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
func (r *StorageNodeOpsReconciler) enterInitialStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
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
	r.emit(ctx, ops, corev1.EventTypeNormal, OperationStarted,
		fmt.Sprintf("The operation acquired the lock on node %s and started", ops.Spec.NodeRef))
	return ctrl.Result{RequeueAfter: opsAdvance}, nil
}

// nextStep is the step that follows the current one. Every graph in this package
// is a line, so the first edge is the only edge.
func (r *StorageNodeOpsReconciler) nextStep(
	machine *statemachine.Machine[step],
) (step, error) {
	current := machine.CurrentState()
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
// leave nothing driving the node back to a state somebody can reason about.
// Promoting is the clearest case: the promote has re-homed the logical volumes,
// so there is nothing to unwind and the operation is what finishes the relocation
// (§9).
func (r *StorageNodeOpsReconciler) unwind(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	machine *statemachine.Machine[step],
	current step,
) (ctrl.Result, error) {
	// The machine is asked rather than a table beside it, and it is asked rather
	// than the graphs, because it was built for this operation's action: a step
	// two actions share can be abortable in one of them.
	if !machine.CanAbort() {
		// Not a failure of the operation: it carries on. What the user asked for
		// cannot be done, and saying so is the whole of the response.
		return ctrl.Result{RequeueAfter: opsRetry}, r.note(ctx, ops, fmt.Sprintf(
			"the abort arrived at step %s, which the control plane is part-way through "+
				"and cannot be stopped; the operation is running on", current))
	}

	// Every terminal outcome from Suspending onward resumes the node first. A
	// node past the suspend is not serving, and an operation that stopped there
	// and left it that way would take capacity out of the cluster for as long as
	// nobody noticed (§8.3).
	r.resumeNode(ctx, ops, current)
	r.abortMigrations(ctx, ops, current)

	r.emit(ctx, ops, corev1.EventTypeNormal, OperationAborted,
		fmt.Sprintf("The operation was aborted at step %s", current))
	return r.finish(ctx, ops, simplyblockv1alpha2.StorageNodeOpsPhaseAborted,
		fmt.Sprintf("aborted at step %s", current))
}

// fail ends the operation, resuming the node first where the step it failed on
// left it suspended.
func (r *StorageNodeOpsReconciler) fail(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	current step,
	message string,
) (ctrl.Result, error) {
	r.resumeNode(ctx, ops, current)
	return r.finish(ctx, ops, simplyblockv1alpha2.StorageNodeOpsPhaseFailed, message)
}

// resumeNode is the unwind of §8.3, and it is best-effort on purpose.
//
// A resume that itself fails leaves the node suspended, which is visible in
// status.status and in the NodeResumeFailed event. Retrying it forever would mean
// an operation that can never reach a terminal phase and a lock that is never
// released, and §16 Q3 is whether that is the right trade.
func (r *StorageNodeOpsReconciler) resumeNode(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, current step,
) {
	if !unwinds(current) {
		return
	}
	clusterID, nodeID, err := r.target(ctx, ops)
	if err != nil || nodeID == "" {
		return
	}
	if err := r.API.Resume(ctx, clusterID, nodeID); err != nil {
		r.emit(ctx, ops, corev1.EventTypeWarning, NodeResumeFailed, fmt.Sprintf(
			"Node %s could not be resumed and is left suspended: %v", ops.Spec.NodeRef, err))
	}
}

// waitOn requeues for whatever is left of the current step's deadline, so that a
// step with a long budget is looked at when it expires rather than on a fixed
// interval, and a step with a short one is not left waiting past it.
func (r *StorageNodeOpsReconciler) waitOn(machine *statemachine.Machine[step]) ctrl.Result {
	if remaining, bounded := machine.RequeueAfter(); bounded && remaining < opsRetry {
		return ctrl.Result{RequeueAfter: remaining}
	}
	return ctrl.Result{RequeueAfter: opsRetry}
}

// finish writes a terminal phase, releases the node's lock, and records what the
// operation cost.
func (r *StorageNodeOpsReconciler) finish(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	phase simplyblockv1alpha2.StorageNodeOpsPhase,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
		status.Phase = phase
		status.Message = message
		status.CompletedAt = &now
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	switch phase {
	case simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded:
		r.emit(ctx, ops, corev1.EventTypeNormal, OperationSucceeded, message)
	case simplyblockv1alpha2.StorageNodeOpsPhaseFailed:
		r.emit(ctx, ops, corev1.EventTypeWarning, OperationFailed, message)
	}

	r.observeOperation(ctx, ops, phase)
	return ctrl.Result{}, r.releaseLock(ctx, ops)
}

// observeOperation records the operation's outcome and how long it ran.
func (r *StorageNodeOpsReconciler) observeOperation(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	phase simplyblockv1alpha2.StorageNodeOpsPhase,
) {
	cluster, act, result := r.clusterLabel(ctx, ops), string(ops.Spec.Action), resultOf(phase)
	operationsTotal.WithLabelValues(cluster, act, result).Inc()
	if started := ops.Status.StartedAt; started != nil {
		operationDurationSeconds.WithLabelValues(cluster, act, result).
			Observe(time.Since(started.Time).Seconds())
	}
	drainBlockedVolumesCount.DeleteLabelValues(cluster, blockedPinned)
	drainBlockedVolumesCount.DeleteLabelValues(cluster, blockedUnmanaged)
}

// observeStep records how long one step took. The start is the step's entry,
// which is its deadline minus the budget the graph gives it, so no second
// timestamp has to be persisted for a measurement.
func (r *StorageNodeOpsReconciler) observeStep(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, current step,
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
	cluster := r.clusterLabel(ctx, ops)
	operationStepDurationSeconds.
		WithLabelValues(cluster, string(ops.Spec.Action), string(current)).Observe(elapsed)
	if current == stepHolding {
		maintenanceHoldSeconds.WithLabelValues(cluster).Observe(elapsed)
	}
}

// The metric labels a terminal phase is reported under, lowercased because a
// label value is not an API enum. They are this package's own vocabulary rather
// than the control plane's, which is why they are not the device status constants
// beside them that happen to spell one of the words the same way.
const (
	resultSucceeded = "succeeded"
	resultAborted   = "aborted"
	resultFailed    = "failed"
)

// resultOf is the metric label for a terminal phase.
func resultOf(phase simplyblockv1alpha2.StorageNodeOpsPhase) string {
	switch phase {
	case simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded:
		return resultSucceeded
	case simplyblockv1alpha2.StorageNodeOpsPhaseAborted:
		return resultAborted
	default:
		return resultFailed
	}
}

// teardown releases the lock, aborts whatever the operation fanned out, and lets
// the object go.
//
// It is the third release path and the one that matters most, because
// `kubectl delete` on a running operation would otherwise leave the node locked
// by an object that no longer exists. Aborting the fan-out first is what stops a
// deleted drain leaving migrations running behind it (§8.4).
func (r *StorageNodeOpsReconciler) teardown(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ops, OpsFinalizer) {
		return ctrl.Result{}, nil
	}
	if pending, err := r.cascadeMigrations(ctx, ops); err != nil {
		return ctrl.Result{}, err
	} else if pending {
		return ctrl.Result{RequeueAfter: opsRetry}, nil
	}
	if err := r.releaseLock(ctx, ops); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(ops, OpsFinalizer)
	return ctrl.Result{}, r.Update(ctx, ops)
}

// acquireLock takes the node's status.activeOpsRef, and reports whether this
// operation now holds it.
//
// Acquisition is an optimistic-lock patch rather than a plain one, which is what
// makes the read-then-write safe: two operations can both read an empty field and
// both conclude the lock is free, and the patch succeeds for exactly one of them
// at a given resourceVersion and returns 409 to the rest (§11).
func (r *StorageNodeOpsReconciler) acquireLock(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (bool, error) {
	node, err := r.node(ctx, ops)
	if apierrors.IsNotFound(err) {
		_, err := r.finish(ctx, ops, simplyblockv1alpha2.StorageNodeOpsPhaseFailed,
			fmt.Sprintf("StorageNode %s does not exist", ops.Spec.NodeRef))
		return false, err
	}
	if err != nil {
		return false, err
	}

	if held := node.Status.ActiveOpsRef; held != "" && held != ops.Name {
		r.emit(ctx, ops, corev1.EventTypeNormal, OperationQueued, fmt.Sprintf(
			"Node %s is held by operation %s; this one is waiting", node.Name, held))
		return false, r.hold(ctx, ops, fmt.Sprintf(
			"waiting for operation %s to release node %s", held, node.Name))
	}

	if node.Status.ActiveOpsRef != ops.Name {
		patch := client.MergeFromWithOptions(node.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		node.Status.ActiveOpsRef = ops.Name
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			if apierrors.IsConflict(err) {
				// Somebody else moved the object between the read and the write.
				// Whether that was another operation taking the lock is decided
				// by reading it again rather than guessed at here.
				return false, nil
			}
			return false, fmt.Errorf("acquire the lock on node %s: %w", node.Name, err)
		}
		operationActiveState.WithLabelValues(node.Spec.ClusterRef, node.Name).Set(1)
	}

	if ops.Status.Phase == "" || ops.Status.Phase == simplyblockv1alpha2.StorageNodeOpsPhasePending {
		now := metav1.Now()
		// How long the operation waited behind another one's lock. The object's
		// creation is the start, because Pending is where an operation both
		// begins and waits.
		operationLockWaitSeconds.WithLabelValues(node.Spec.ClusterRef, string(ops.Spec.Action)).
			Observe(now.Sub(ops.CreationTimestamp.Time).Seconds())
		err := r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
			status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseRunning
			status.StartedAt = &now
			status.Message = "The operation holds the node and is running"
		})
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

// releaseLock clears the node's status.activeOpsRef, but only while it still
// names this operation.
//
// The ownership check is what makes a late release safe. A pass that started
// before the lock changed hands would otherwise clear a lock somebody else now
// holds, which is worse than not releasing at all: two operations would then be
// running against one node with neither of them knowing.
func (r *StorageNodeOpsReconciler) releaseLock(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) error {
	node, err := r.node(ctx, ops)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if node.Status.ActiveOpsRef != ops.Name {
		return nil
	}

	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Status.ActiveOpsRef = ""
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		// A conflict is reported rather than swallowed, and that is the whole
		// point of returning an error here. Somebody else wrote the node's status
		// between the read and the write, so the lock this operation still holds
		// was not cleared; treating that as a release lets the caller reach a
		// terminal phase and the finalizer go, and the node stays locked by an
		// object that no longer exists. Reporting it retries on the next pass.
		return fmt.Errorf("release the lock on node %s: %w", node.Name, err)
	}
	operationActiveState.WithLabelValues(node.Spec.ClusterRef, node.Name).Set(0)
	return nil
}

// node reads the operation's target.
func (r *StorageNodeOpsReconciler) node(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (*simplyblockv1alpha2.StorageNode, error) {
	var node simplyblockv1alpha2.StorageNode
	key := types.NamespacedName{Name: ops.Spec.NodeRef, Namespace: ops.Namespace}
	if err := r.Get(ctx, key, &node); err != nil {
		return nil, err
	}
	return &node, nil
}

// target resolves the operation to the pair every control-plane call takes: the
// cluster's UUID and the backend node's.
func (r *StorageNodeOpsReconciler) target(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (clusterID, nodeID string, err error) {
	node, err := r.node(ctx, ops)
	if err != nil {
		return "", "", err
	}
	if node.Status.UUID == "" {
		return "", "", fatalf("node %s has not been provisioned in the control plane yet",
			ops.Spec.NodeRef)
	}
	var cluster simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Name: node.Spec.ClusterRef, Namespace: node.Namespace}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return "", "", err
	}
	if cluster.Status.UUID == "" {
		return "", "", fatalf("cluster %s has not been created in the control plane yet",
			node.Spec.ClusterRef)
	}
	return cluster.Status.UUID, node.Status.UUID, nil
}

// clusterReady is the gate of §7.1: a node operation runs inside a cluster, and
// one whose cluster is not active or is mid-rebalance will either be rejected by
// the control plane or succeed into an inconsistent layout.
//
// It reports a reason rather than an error, because holding is the response and a
// reason is what an event carries. A cluster whose reading cannot be taken at all
// is an error, which the caller retries.
// skipsClusterGate reports the operations that run whatever the cluster says
// about itself.
//
// Removal is the one. A node is removed from an unready cluster precisely to
// make the cluster ready, so holding the removal until the cluster is active
// closes a loop with no way out: the node cannot be removed until the cluster is
// active, and the cluster cannot become active while the node it is stuck on is
// still in it. That is not hypothetical — it is what a node whose add never
// finished does to the cluster it was being added to.
//
// Nothing else is exempt. The gate exists to keep an operation that moves data
// off a cluster that cannot take it, and an exemption wider than the one case
// that needs it is a gate that stops meaning anything.
func skipsClusterGate(ops *simplyblockv1alpha2.StorageNodeOps) bool {
	return ops.Spec.Action == simplyblockv1alpha2.StorageNodeOpsActionRemove
}

func (r *StorageNodeOpsReconciler) clusterReady(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) (bool, string, error) {
	node, err := r.node(ctx, ops)
	if err != nil {
		return false, "", err
	}
	var cluster simplyblockv1alpha2.StorageCluster
	key := types.NamespacedName{Name: node.Spec.ClusterRef, Namespace: node.Namespace}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return false, "", err
	}
	if cluster.Status.UUID == "" {
		return false, fmt.Sprintf(
			"cluster %s has not been created in the control plane yet", cluster.Name), nil
	}

	// The cache is trusted only once the one cluster stream has delivered its
	// snapshot. A cluster missing from an unsynced cache and one the control
	// plane has forgotten look identical, and reading the first as the second
	// would hold every node operation in the deployment.
	if r.Clusters == nil || !r.Clusters.SyncedRoot() {
		return true, "", nil
	}
	reading, ok := r.Clusters.Lookup(cluster.Status.UUID)
	if !ok {
		return false, fmt.Sprintf(
			"the control plane no longer reports cluster %s", cluster.Name), nil
	}
	if reading.Status != utils.ClusterStatusActive {
		return false, fmt.Sprintf("cluster %s is %s rather than active",
			cluster.Name, reading.Status), nil
	}
	if reading.Rebalancing {
		return false, fmt.Sprintf(
			"cluster %s is rebalancing; the operation resumes when it settles", cluster.Name), nil
	}
	return true, "", nil
}

// clusterLabel is the metric label for the operation's cluster. It is the object
// name rather than the UUID, matching every other series in this package, and it
// is empty for an operation whose node has gone rather than an error: a metric is
// not worth failing a reconcile over.
func (r *StorageNodeOpsReconciler) clusterLabel(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps,
) string {
	node, err := r.node(ctx, ops)
	if err != nil {
		return ""
	}
	return node.Spec.ClusterRef
}

// nodeReading is what the control plane currently says about the node, from the
// stream's cache once it has delivered its snapshot and from the control plane
// until then.
//
// An operation asks this on every pass of every step, so a drain that takes an
// hour is hundreds of reads the informer is already holding. The gate is the
// snapshot rather than a preference, because a node missing from an unsynced
// cache and one the control plane has forgotten look identical, and reading the
// first as the second would report a shutdown complete while the node is still up.
func (r *StorageNodeOpsReconciler) nodeReading(
	ctx context.Context, clusterID, nodeID string,
) (NodeReading, error) {
	if r.Nodes != nil && r.Nodes.Synced(cpinformer.Scope{clusterID}) {
		if _, dto, ok := r.Nodes.Lookup(nodeID); ok {
			return readingFromDTO(dto), nil
		}
		return NodeReading{}, fmt.Errorf("the control plane no longer reports node %s", nodeID)
	}

	reading, ok, err := r.API.StorageNode(ctx, clusterID, nodeID)
	if err != nil {
		return NodeReading{}, fmt.Errorf("read node %s: %w", nodeID, err)
	}
	if !ok {
		return NodeReading{}, fmt.Errorf("the control plane no longer reports node %s", nodeID)
	}
	return reading, nil
}

// readingFromDTO projects a streamed node onto the reading every predicate in
// this package is written against. The stream carries less than the list does —
// no device counts, no memory, and no uptime — so those stay at their zero values
// and no completion condition reads them.
func readingFromDTO(dto subscriptions.NodeDTO) NodeReading {
	return NodeReading{
		UUID:          dto.ID,
		Status:        dto.Status,
		ManagementIP:  dto.ManagementIP,
		Health:        dto.HealthCheck,
		Hostname:      dto.Hostname,
		CPUCount:      dto.CPUCount,
		Volumes:       dto.Volumes,
		RPCPort:       dto.RPCPort,
		LvolPort:      dto.LvolPort,
		NVMeOFPort:    dto.NVMeOFPort,
		FailureDomain: dto.FailureDomain,
	}
}

// hold reports an operation that is admitted, holds nothing, and is waiting.
// Pending is both where an operation starts and where it waits, and the
// OperationQueued event is the only thing that separates the two.
func (r *StorageNodeOpsReconciler) hold(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
		if status.Phase == "" {
			status.Phase = simplyblockv1alpha2.StorageNodeOpsPhasePending
		}
		status.Message = message
	})
}

// note replaces status.message without moving anything else. It is one sentence
// about where the operation is, replaced as it moves, and never a log.
func (r *StorageNodeOpsReconciler) note(
	ctx context.Context, ops *simplyblockv1alpha2.StorageNodeOps, message string,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
		status.Message = message
	})
}

// recordStep persists the step the operation is about to be in, with the instant
// it expires. Both travel together, because a step persisted without its deadline
// restores as a step that can never time out.
func (r *StorageNodeOpsReconciler) recordStep(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	next step,
	deadline *metav1.Time,
) error {
	return r.writeStatus(ctx, ops, func(status *simplyblockv1alpha2.StorageNodeOpsStatus) {
		status.Phase = simplyblockv1alpha2.StorageNodeOpsPhaseRunning
		status.Step = statemachine.KubeSnapshot{State: string(next), Deadline: deadline}
	})
}

// writeStatus applies the mutation and patches only when something changed.
//
// observedGeneration is written here rather than by each caller, and on this kind
// its second advance is precisely the signal that spec.abort has been observed: a
// user who sets it and sees an unchanged status cannot otherwise tell a controller
// that has not looked from one that looked and declined.
func (r *StorageNodeOpsReconciler) writeStatus(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	mutate func(*simplyblockv1alpha2.StorageNodeOpsStatus),
) error {
	// Retried rather than swallowed on a conflict. A caller that read nil would
	// take the write for done, and finish does: it releases the node's lock
	// straight afterward, so a dropped terminal status would free the node for
	// the next operation while this one still reported Running.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.StorageNodeOps
		if err := r.Get(ctx, client.ObjectKeyFromObject(ops), &fresh); err != nil {
			return err
		}

		desired := *fresh.Status.DeepCopy()
		mutate(&desired)
		desired.ObservedGeneration = fresh.Generation

		if equalOpsStatus(fresh.Status, desired) {
			// Still published to the caller, which reads the object it passed in
			// on the next line of its own logic.
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

// emit raises an event on the operation and mirrors it onto the target node.
//
// The mirror is not duplication. An operation's events belong on the object that
// outlives it as its audit record, and the node is where somebody investigating a
// stuck cluster starts: they have the worker's name and not the operation's
// (§13.1).
func (r *StorageNodeOpsReconciler) emit(
	ctx context.Context,
	ops *simplyblockv1alpha2.StorageNodeOps,
	eventType, reason, message string,
) {
	r.Recorder.Eventf(ops, nil, eventType, reason, reason, "%s", message)
	if node, err := r.node(ctx, ops); err == nil {
		r.Recorder.Eventf(node, nil, eventType, reason, reason, "%s", message)
	}
}

// terminalOps reports a phase the operation can never leave.
func terminalOps(phase simplyblockv1alpha2.StorageNodeOpsPhase) bool {
	switch phase {
	case simplyblockv1alpha2.StorageNodeOpsPhaseSucceeded,
		simplyblockv1alpha2.StorageNodeOpsPhaseFailed,
		simplyblockv1alpha2.StorageNodeOpsPhaseAborted:
		return true
	default:
		return false
	}
}

// terminalStepError is a step failure that retrying cannot fix: an action the
// control plane refused outright, a node with no UUID, a regular expression that
// does not compile. It is a distinct type so that the reconcile loop can tell it
// from a control plane that is briefly unreachable, which is the same shape of
// error and the opposite response.
type terminalStepError struct{ reason string }

func (e *terminalStepError) Error() string { return e.reason }

func fatalf(format string, args ...any) error {
	return &terminalStepError{reason: fmt.Sprintf(format, args...)}
}

// blockedStepError is a step that cannot proceed and has not gone wrong: a drain
// held by a pinned claim, one with no online peer to move to, a maintenance
// window waiting for its concurrency slot.
//
// It is a distinct type because the response is the opposite of a failure's. The
// operation holds, keeps its deadline running, and emits the reason it carries,
// because correct behavior that looks exactly like a stalled controller is what
// the event surface exists for (§13.1).
type blockedStepError struct {
	reason  string
	message string
}

func (e *blockedStepError) Error() string { return e.message }

func blockedf(reason, format string, args ...any) error {
	return &blockedStepError{reason: reason, message: fmt.Sprintf(format, args...)}
}

// equalOpsStatus compares two statuses for the purpose of deciding whether to
// write. It is spelled out rather than reflect.DeepEqual because the status
// carries pointers to timestamps, and two equal instants behind two pointers are
// not deeply equal.
func equalOpsStatus(a, b simplyblockv1alpha2.StorageNodeOpsStatus) bool {
	if a.Phase != b.Phase || a.Message != b.Message ||
		a.ObservedGeneration != b.ObservedGeneration ||
		a.Step.State != b.Step.State ||
		!equalTime(a.Step.Deadline, b.Step.Deadline) ||
		!equalTime(a.StartedAt, b.StartedAt) ||
		!equalTime(a.CompletedAt, b.CompletedAt) {
		return false
	}
	if (a.Drain == nil) != (b.Drain == nil) {
		return false
	}
	if a.Drain == nil {
		return true
	}
	return *a.Drain == *b.Drain
}

func equalTime(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

// successMessage is what a finished operation says it did. A drain has its own,
// because how many volumes moved is the only number a reader wants from it.
func (r *StorageNodeOpsReconciler) successMessage(
	ops *simplyblockv1alpha2.StorageNodeOps,
) string {
	if ops.Spec.Action == simplyblockv1alpha2.StorageNodeOpsActionRemove {
		moved := int32(0)
		if d := ops.Status.Drain; d != nil {
			moved = d.VolumesMigrated
		}
		return fmt.Sprintf("node %s removed after moving %d volumes", ops.Spec.NodeRef, moved)
	}
	return fmt.Sprintf("the %s completed on node %s", ops.Spec.Action, ops.Spec.NodeRef)
}

// waitingMessage is what an unfinished step says. A drain's says how far through
// its volumes it is as well as which step, because the step alone does not locate
// a drain that runs for hours.
func (r *StorageNodeOpsReconciler) waitingMessage(
	ops *simplyblockv1alpha2.StorageNodeOps, current step,
) string {
	if d := ops.Status.Drain; d != nil && current == stepMigratingVolumes {
		return fmt.Sprintf("%d of %d volumes migrated", d.VolumesMigrated, d.VolumesTotal)
	}
	return fmt.Sprintf("waiting on %s", current)
}
