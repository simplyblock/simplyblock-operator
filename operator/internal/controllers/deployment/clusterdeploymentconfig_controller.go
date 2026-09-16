// The ClusterDeploymentConfig reconciler: it validates a draft on every pass and
// expands an approved one into a StorageCluster and its StorageNodes.
//
// The document is ephemeral, and everything below follows from that. It owns
// nothing, nothing references it, and nothing reads it after the expansion, so
// there is no finalizer and no cleanup: deleting it deletes a document. It
// specifically does not own the StorageCluster it created, because an owner
// reference would make deleting the document delete the cluster and every volume
// in it.
//
// A draft is validated on every reconcile and expanded on none. Validation writes
// what it found into status.message and nothing else, so a reviewer sees the
// problems before approving rather than after — which is the whole value of the
// gate, and why admission lets a draft naming a missing worker be saved at all.
//
// The expansion is create-only (§6). It creates a cluster or adds nodes to one,
// and it never reconciles a difference: a document that names an existing cluster
// without asking to is a failure with a reason, because the differences that
// matter here are of the form: this node's device list changed, and its only
// correct handling is not to apply it to a node that already has data on those
// devices.
//
// design-clusterdeploymentconfig.md §4 is the specification.

package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// readyToDeploy marks an approved document so a selector can find one. The
	// operator writes it and reads it from nowhere: it is an output (§5).
	//
	// It is a label rather than an annotation, and that is the whole point of it.
	// A label selector cannot see an annotation, so the marker would have been
	// invisible to the one thing it exists for.
	readyToDeploy = "storage.simplyblock.io/ready-to-deploy"

	// readyToDeployValue is what the marker carries. An annotation value is a
	// string, and this is the one a selector matches on.
	readyToDeployValue = "true"

	// configRetry is how long a held document waits before looking again at
	// something it cannot hurry: a control plane that is not ready, or a worker
	// somebody has yet to add.
	configRetry = 30 * time.Second

	// configAdvance is how long a pass that moved the machine forward waits. The
	// status write this pass made is itself a change the controller watches, so
	// this is the backstop for the event rather than the path the next step
	// normally arrives on.
	configAdvance = time.Second
)

// ClusterDeploymentConfigReconciler reconciles a ClusterDeploymentConfig.
type ClusterDeploymentConfigReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Namespace is where the operator runs, which is where the ControlPlane
	// singleton the expansion waits on lives.
	Namespace string
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=clusterdeploymentconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=clusterdeploymentconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusterops,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// SetupWithManager registers the controller.
//
// It watches Kubernetes Nodes as well as its own kind, because a draft held by a
// worker that does not exist becomes valid the moment somebody adds it, and
// waiting out a requeue interval to notice would make the gate feel broken.
func (r *ClusterDeploymentConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.ClusterDeploymentConfig{}).
		Named("clusterdeploymentconfig").
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.everyUnfinishedDocument)).
		Complete(r)
}

func (r *ClusterDeploymentConfigReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	var config simplyblockv1alpha2.ClusterDeploymentConfig
	if err := r.Get(ctx, req.NamespacedName, &config); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A deleted document needs nothing done to it. There is no finalizer, because
	// it owns nothing and nothing reads it (§4.3).
	if !config.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Expanded and Failed are terminal. A document is the record of one
	// deployment action, so re-running it is writing another document rather than
	// editing this one.
	if terminalConfig(config.Status.Phase) {
		return ctrl.Result{}, nil
	}

	findings, err := r.validate(ctx, &config)
	if err != nil {
		return ctrl.Result{RequeueAfter: configRetry}, err
	}

	if !config.Spec.Approved {
		return r.holdAsDraft(ctx, &config, findings)
	}

	// An approved document that does not validate is a failure rather than a
	// hold. Approval is what makes it immutable, so a problem found after it can
	// no longer be edited away, and holding forever would say less than failing.
	if len(findings) > 0 {
		r.emitFindings(&config, findings)
		return r.fail(ctx, &config, findings[0].message)
	}

	if err := r.markReadyToDeploy(ctx, &config); err != nil {
		return ctrl.Result{}, err
	}

	if ready, reason := r.controlPlaneReady(ctx); !ready {
		// Expanding rather than Draft. The document is approved, and Draft is
		// what the API calls one that is not: reporting it would tell every
		// status consumer the deployment is still editable and has not started.
		r.emit(&config, corev1.EventTypeWarning, ControlPlaneNotReady, reason)
		return ctrl.Result{RequeueAfter: configRetry}, r.note(ctx, &config,
			simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding, reason)
	}

	return r.expand(ctx, &config)
}

// expand runs the machine of §4.2 forward by at most one step.
func (r *ClusterDeploymentConfigReconciler) expand(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (ctrl.Result, error) {
	machine, err := statemachine.NewFromSnapshot(ctx, expansionGraph(),
		statemachine.FromKube[configStep](config.Status.Step))
	if err != nil {
		// An unrecognized step is a downgrade, a hand-edited object, or a rename
		// that shipped without a conversion, and none of them resolve by
		// reconciling again.
		return r.fail(ctx, config, fmt.Sprintf("the expansion cannot be resumed: %v", err))
	}
	defer machine.Close()

	// A machine is born already in its initial state, so that state's entry hook
	// never runs and no deadline is set for it. Setting one on the first pass is
	// what stops the first step being the one step that cannot time out.
	if config.Status.Step.State == "" {
		deadline := metav1.NewTime(time.Now().Add(validatingDeadline))
		return ctrl.Result{RequeueAfter: configAdvance},
			r.recordStep(ctx, config, machine.CurrentState(), &deadline)
	}

	current := machine.CurrentState()
	if machine.TimeoutReached() {
		r.emit(config, corev1.EventTypeWarning, StepDeadlineExceeded,
			fmt.Sprintf("Step %s outlived its deadline", current))
		return r.fail(ctx, config, fmt.Sprintf("step %s outlived its deadline", current))
	}

	done, err := r.performStep(ctx, config, current)
	if err != nil {
		var refusal *refusedError
		if errors.As(err, &refusal) {
			r.emit(config, corev1.EventTypeWarning, refusal.reason, refusal.Error())
			return r.fail(ctx, config, refusal.Error())
		}
		logf.FromContext(ctx).Error(err, "the expansion step could not be advanced",
			"config", config.Name, "step", current)
		return ctrl.Result{RequeueAfter: configRetry}, r.note(ctx, config,
			simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding, err.Error())
	}
	if !done {
		return ctrl.Result{RequeueAfter: configRetry}, r.note(ctx, config,
			simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding,
			fmt.Sprintf("waiting on %s", current))
	}

	if machine.IsTerminal() {
		// The expansion does not wait for the nodes to come up. It created the
		// objects and is finished; provisioning them is the node controller's and
		// is bounded by maxParallelNodeAdds, and a document that stayed Expanding
		// until a twenty-node fleet was online would be reporting the fleet's
		// progress rather than its own (§4.2).
		return ctrl.Result{}, r.succeed(ctx, config)
	}

	next, err := nextStep(machine)
	if err != nil {
		return r.fail(ctx, config, err.Error())
	}
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	snapshot := statemachine.ToKube(machine.Snapshot())
	return ctrl.Result{RequeueAfter: configAdvance},
		r.recordStep(ctx, config, next, snapshot.Deadline)
}

// performStep runs one step and reports whether it has finished.
func (r *ClusterDeploymentConfigReconciler) performStep(
	ctx context.Context,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	current configStep,
) (bool, error) {
	switch current {
	case stepValidating:
		// Validation already ran on the way in, and reaching here means it found
		// nothing. The step exists so that the machine's first position is the
		// check rather than a side effect.
		return true, nil
	case stepCreatingCluster:
		return r.createCluster(ctx, config)
	case stepAwaitingCluster:
		return r.awaitCluster(ctx, config)
	case stepCreatingNodes:
		return r.createNodes(ctx, config)
	case stepActivating:
		return r.activateCluster(ctx, config)
	default:
		return false, fmt.Errorf("step %s belongs to no expansion this operator runs", current)
	}
}

// holdAsDraft reports what validation found and leaves the document alone.
//
// A draft is expanded on no reconcile, so this is the whole of what happens to one
// until somebody approves it.
func (r *ClusterDeploymentConfigReconciler) holdAsDraft(
	ctx context.Context,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	findings []finding,
) (ctrl.Result, error) {
	if len(findings) > 0 {
		r.emitFindings(config, findings)
		return ctrl.Result{RequeueAfter: configRetry}, r.note(ctx, config,
			simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft, summarize(findings))
	}

	// AwaitingApproval is emitted on the transition to a validated draft rather
	// than on every reconcile, because a valid draft nobody has approved looks
	// identical to a controller that has not noticed it and the event is what
	// distinguishes them — once.
	message := "the document is valid and is waiting for spec.approved"
	if config.Status.Message != message {
		r.emit(config, corev1.EventTypeNormal, AwaitingApproval, message)
	}
	return ctrl.Result{}, r.note(ctx, config,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft, message)
}

// markReadyToDeploy writes the output annotation of §5. It is set on an approved
// document so a selector can find one, and read from nowhere.
func (r *ClusterDeploymentConfigReconciler) markReadyToDeploy(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) error {
	if config.Labels[readyToDeploy] == readyToDeployValue {
		return nil
	}
	patch := client.MergeFrom(config.DeepCopy())
	if config.Labels == nil {
		config.Labels = map[string]string{}
	}
	config.Labels[readyToDeploy] = readyToDeployValue
	return r.Patch(ctx, config, patch)
}

// controlPlaneReady reports whether the singleton is available, which is the
// precondition for creating a cluster at all.
func (r *ClusterDeploymentConfigReconciler) controlPlaneReady(
	ctx context.Context,
) (bool, string) {
	var controlPlane simplyblockv1alpha2.ControlPlane
	key := types.NamespacedName{Namespace: r.Namespace, Name: singletonControlPlane}
	if err := r.Get(ctx, key, &controlPlane); err != nil {
		return false, fmt.Sprintf("ControlPlane %s cannot be read: %v",
			singletonControlPlane, err)
	}
	if controlPlane.Status.Phase != controlPlaneAvailable {
		return false, fmt.Sprintf("ControlPlane %s is %s rather than %s",
			singletonControlPlane, controlPlane.Status.Phase, controlPlaneAvailable)
	}
	return true, ""
}

// everyUnfinishedDocument maps a Node event onto every document that has not
// finished, because a draft held by a worker that does not exist becomes valid the
// moment somebody adds it.
func (r *ClusterDeploymentConfigReconciler) everyUnfinishedDocument(
	ctx context.Context, _ client.Object,
) []reconcile.Request {
	var configs simplyblockv1alpha2.ClusterDeploymentConfigList
	if err := r.List(ctx, &configs); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range configs.Items {
		if terminalConfig(configs.Items[i].Status.Phase) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&configs.Items[i]),
		})
	}
	return requests
}

// succeed records the expansion's outcome.
func (r *ClusterDeploymentConfigReconciler) succeed(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) error {
	message := fmt.Sprintf("expanded into cluster %s and %d node(s)",
		config.Status.ClusterRef, len(config.Status.NodeRefs))
	r.emit(config, corev1.EventTypeNormal, NodesCreated, message)
	return r.note(ctx, config,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded, message)
}

// fail records a terminal refusal.
func (r *ClusterDeploymentConfigReconciler) fail(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig, message string,
) (ctrl.Result, error) {
	return ctrl.Result{}, r.note(ctx, config,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed, message)
}

// note writes the phase and the message, and patches only when something changed.
func (r *ClusterDeploymentConfigReconciler) note(
	ctx context.Context,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	phase simplyblockv1alpha2.ClusterDeploymentConfigPhase,
	message string,
) error {
	return r.writeStatus(ctx, config,
		func(status *simplyblockv1alpha2.ClusterDeploymentConfigStatus) {
			status.Phase = phase
			status.Message = message
		})
}

// recordStep persists the step the machine is about to be in, with the instant it
// expires. Both travel together, because a step persisted without its deadline
// restores as a step that can never time out.
func (r *ClusterDeploymentConfigReconciler) recordStep(
	ctx context.Context,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	next configStep,
	deadline *metav1.Time,
) error {
	return r.writeStatus(ctx, config,
		func(status *simplyblockv1alpha2.ClusterDeploymentConfigStatus) {
			status.Phase = simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanding
			status.Step = statemachine.KubeSnapshot{
				State: string(next), Deadline: deadline,
			}
		})
}

// writeStatus applies the mutation and patches only when something changed.
func (r *ClusterDeploymentConfigReconciler) writeStatus(
	ctx context.Context,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	mutate func(*simplyblockv1alpha2.ClusterDeploymentConfigStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.ClusterDeploymentConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(config), &fresh); err != nil {
			return err
		}

		desired := *fresh.Status.DeepCopy()
		mutate(&desired)
		desired.ObservedGeneration = fresh.Generation

		if equalConfigStatus(fresh.Status, desired) {
			config.Status = desired
			config.ResourceVersion = fresh.ResourceVersion
			return nil
		}

		patch := client.MergeFromWithOptions(fresh.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		fresh.Status = desired
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		config.Status = fresh.Status
		config.ResourceVersion = fresh.ResourceVersion
		return nil
	})
}

// emit raises an event on the document, which is what a reviewer has open.
func (r *ClusterDeploymentConfigReconciler) emit(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	eventType, reason, message string,
) {
	r.Recorder.Eventf(config, nil, eventType, reason, reason, "%s", message)
}

// emitFindings raises one event per distinct reason validation produced.
func (r *ClusterDeploymentConfigReconciler) emitFindings(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, findings []finding,
) {
	seen := map[string]struct{}{}
	for _, found := range findings {
		if _, already := seen[found.reason]; already {
			continue
		}
		seen[found.reason] = struct{}{}
		r.emit(config, corev1.EventTypeWarning, found.reason, found.message)
	}
}

// terminalConfig reports a phase the document can never leave.
func terminalConfig(phase simplyblockv1alpha2.ClusterDeploymentConfigPhase) bool {
	switch phase {
	case simplyblockv1alpha2.ClusterDeploymentConfigPhaseExpanded,
		simplyblockv1alpha2.ClusterDeploymentConfigPhaseFailed:
		return true
	default:
		return false
	}
}

// equalConfigStatus compares two statuses for the purpose of deciding whether to
// write. It is spelled out rather than reflect.DeepEqual because the status
// carries a pointer to a timestamp and a slice.
func equalConfigStatus(a, b simplyblockv1alpha2.ClusterDeploymentConfigStatus) bool {
	if a.Phase != b.Phase || a.Message != b.Message ||
		a.ClusterRef != b.ClusterRef ||
		a.ObservedGeneration != b.ObservedGeneration ||
		a.Step.State != b.Step.State ||
		len(a.NodeRefs) != len(b.NodeRefs) {
		return false
	}
	if (a.Step.Deadline == nil) != (b.Step.Deadline == nil) {
		return false
	}
	if a.Step.Deadline != nil && !a.Step.Deadline.Equal(b.Step.Deadline) {
		return false
	}
	for i := range a.NodeRefs {
		if a.NodeRefs[i] != b.NodeRefs[i] {
			return false
		}
	}
	return true
}

// refusedError is an expansion the document asked for and the operator will not
// perform: the cluster already exists, or the one it names does not. It is a
// distinct type because retrying cannot change either answer, and §6 is explicit
// that refusing beats merging.
type refusedError struct {
	reason  string
	message string
}

func (e *refusedError) Error() string { return e.message }

func refusef(reason, format string, args ...any) error {
	return &refusedError{reason: reason, message: fmt.Sprintf(format, args...)}
}

// The ControlPlane the expansion waits on, and the phase it waits for.
const (
	singletonControlPlane = "simplyblock"
	controlPlaneAvailable = "Available"
)
