// The ControlPlane reconciler: the installation machine, the steady state it
// settles into, and the finalizer that refuses while clusters still exist.
//
// Nothing here blocks. A step that is not finished requeues, and the step it is
// on is in the status, so a controller restart resumes rather than restarts.
//
// Two paths leave Reconcile, and they are different in what the operator owns.
// A managed control plane is installed, watched, and re-applied on every pass,
// which is what puts back an object somebody deleted and corrects one somebody
// edited — and it is why no operation exists for checking the install. An
// external one is resolved, probed, and reported, and the operator touches
// nothing behind its endpoint.
//
// design-controlplane.md §4 is the specification.

package controlplane

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/simplyblock/atlas/statemachine"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// FinalizerControlPlane is what holds a deletion while any StorageCluster in
	// the namespace still exists, and what gives the controller a pass to delete
	// the cluster-scoped objects the garbage collector will not.
	FinalizerControlPlane = "storage.simplyblock.io/controlplane-finalizer"

	// steadyStateInterval is how often a settled control plane is probed. It is
	// also the resolution at which an outage is noticed, which is the reason it
	// is not longer.
	steadyStateInterval = 30 * time.Second

	// installAdvance is how long a pass that moved the machine forward waits.
	// The status write this pass made is itself a change the controller watches,
	// so this is the backstop for the event rather than the path the next step
	// normally arrives on.
	installAdvance = time.Second

	// installRetry is how long a held installation step waits before looking
	// again at something it cannot hurry.
	installRetry = 15 * time.Second
)

// ControlPlaneReconciler reconciles the singleton ControlPlane.
type ControlPlaneReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Prober performs the readiness and version reads. It is an interface so the
	// phase branches can be exercised without an HTTP server.
	Prober Prober
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=controlplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;configmaps;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete;escalate;bind
// +kubebuilder:rbac:groups=apps.foundationdb.org,resources=foundationdbclusters;foundationdbbackups,verbs=get;list;watch;create;update;patch;delete

// Reconcile advances the control plane by at most one step.
func (r *ControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// A ControlPlane under another name is ignored and sits inert, which is the
	// singleton enforced by convention rather than by the API server (§3.1).
	if req.Name != SingletonName {
		log.Info("ignoring a ControlPlane that is not the singleton", "name", req.Name)
		return ctrl.Result{}, nil
	}

	var cp simplyblockv1alpha2.ControlPlane
	if err := r.Get(ctx, req.NamespacedName, &cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cp.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cp)
	}

	// The limit is one per Kubernetes cluster and not one per namespace (§3.1),
	// and the name check above cannot see that: it admits a "simplyblock" object
	// in every namespace. Each of those would apply the same fixed-name
	// cluster-scoped roles and bindings under the same managed-by label, so they
	// would overwrite each other's and either one's finalizer would delete what
	// the other needs.
	//
	// The check runs before the finalizer is taken, for the reason
	// SimplyblockDriver gives for the same ordering: a finalizer on the duplicate
	// would delete the holder's cluster-scoped objects when somebody removed the
	// duplicate, which is the opposite of what removing a duplicate should do.
	holder, err := r.deploymentHolder(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	// An empty holder means the list came back without the object this reconcile
	// just read, which is a stale cache rather than a second control plane.
	// Refusing on it would stall an install behind a message naming nobody, so
	// the object in hand is taken as the holder and the next pass corrects it.
	if holder.Name != "" && holder != client.ObjectKeyFromObject(&cp) {
		message := fmt.Sprintf(
			"a Kubernetes cluster holds one ControlPlane, and %s/%s holds it",
			holder.Namespace, holder.Name)
		r.emit(&cp, corev1.EventTypeWarning, DuplicateControlPlane, message)
		return ctrl.Result{RequeueAfter: steadyStateInterval}, r.report(ctx, &cp,
			simplyblockv1alpha2.ControlPlanePhaseInstalling, message)
	}

	if err := r.ensureFinalizer(ctx, &cp); err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case isManaged(&cp):
		return r.reconcileExternal(ctx, &cp)
	case isLocal(&cp):
		return r.reconcileManaged(ctx, &cp)
	default:
		// The API's CEL rule refuses this at admission, so reaching it means an
		// object written before the rule shipped or one a conversion produced.
		// It holds and says so rather than picking a mode on the user's behalf.
		return ctrl.Result{RequeueAfter: steadyStateInterval}, r.report(ctx, &cp,
			simplyblockv1alpha2.ControlPlanePhaseInstalling,
			"spec.source names neither a managed nor a remote control plane, so there is "+
				"nothing to install and nowhere to probe")
	}
}

// reconcileExternal resolves the endpoint, probes it, and reports. The operator
// installs nothing and owns no components here, so status.components stays empty
// and the probe is the only signal: the phase is Available or Unavailable, and
// Degraded is unreachable (§4.3).
func (r *ControlPlaneReconciler) reconcileExternal(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (ctrl.Result, error) {
	access, err := resolveManaged(ctx, r.Client, cp)
	if err != nil {
		var credentials *credentialsError
		reason := EndpointUnreachable
		if errorsAs(err, &credentials) {
			reason = CredentialsError
		}
		r.emit(cp, corev1.EventTypeWarning, reason, err.Error())
		return ctrl.Result{RequeueAfter: steadyStateInterval}, r.report(ctx, cp,
			simplyblockv1alpha2.ControlPlanePhaseUnavailable, err.Error())
	}

	ok, message := r.probe(ctx, cp.Namespace, access)
	phase := simplyblockv1alpha2.ControlPlanePhaseAvailable
	if !ok {
		phase = simplyblockv1alpha2.ControlPlanePhaseUnavailable
	}

	r.announce(cp, phase, message)
	return ctrl.Result{RequeueAfter: steadyStateInterval}, r.publish(ctx, cp, statusUpdate{
		phase:    phase,
		message:  message,
		endpoint: access.endpoint,
		version:  r.version(ctx, access),
		probed:   true,
	})
}

// reconcileManaged runs the installation machine, and then the steady state it
// settles into.
func (r *ControlPlaneReconciler) reconcileManaged(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (ctrl.Result, error) {
	// The FoundationDB kinds are a prerequisite rather than something to wait
	// on: creating a FoundationDBCluster against a group the API server does not
	// serve is an error on every attempt, and the CRDs are the chart's to apply.
	if served, err := r.foundationDBServed(); err != nil {
		return ctrl.Result{}, err
	} else if !served {
		message := fmt.Sprintf(
			"the Kubernetes cluster does not serve %s/%s, so the FoundationDB this control "+
				"plane stores its state in cannot be created", fdbGroup, fdbVersion)
		r.emit(cp, corev1.EventTypeWarning, PrerequisiteMissing, message)
		return ctrl.Result{RequeueAfter: installRetry}, r.report(ctx, cp,
			simplyblockv1alpha2.ControlPlanePhaseInstalling, message)
	}

	if cp.Status.Phase != simplyblockv1alpha2.ControlPlanePhaseInstalling &&
		cp.Status.Phase != "" {
		return r.steadyState(ctx, cp)
	}
	return r.install(ctx, cp)
}

// install advances the installation machine by at most one step.
func (r *ControlPlaneReconciler) install(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (ctrl.Result, error) {
	machine, err := statemachine.NewFromSnapshot(ctx, installGraph(),
		statemachine.FromKube[installStep](cp.Status.Step))
	if err != nil {
		// An unrecognized step is a downgrade, a hand-edited object, or a rename
		// that shipped without a conversion, and none of them resolve by
		// reconciling again. Restarting the install is the safe answer here and
		// not elsewhere in this group: every step is an apply, so re-entering
		// the first one re-applies to the same result.
		return r.enterStepAt(ctx, cp, stepApplyingFoundationDB)
	}
	defer machine.Close()

	// A machine is born already in its initial state, so that state's entry hook
	// never runs and no deadline is set for it. Setting one on the first pass is
	// what stops the first step being the one step that cannot time out.
	if cp.Status.Step.State == "" {
		return r.enterStepAt(ctx, cp, machine.CurrentState())
	}

	current := machine.CurrentState()

	if machine.TimeoutReached() {
		message := fmt.Sprintf("step %s outlived its deadline", current)
		r.emit(cp, corev1.EventTypeWarning, StepDeadlineExceeded, message)
		// The step is not abandoned. An install that has outrun its budget is
		// still the only path to a working control plane, and there is nothing
		// to roll back to: the deadline is a detection mechanism rather than a
		// recovery one, and what it produces is the event and the message.
		return ctrl.Result{RequeueAfter: installRetry}, r.report(ctx, cp,
			simplyblockv1alpha2.ControlPlanePhaseInstalling, message)
	}

	done, held, err := r.performInstallStep(ctx, cp, current)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		r.emit(cp, corev1.EventTypeNormal, AwaitingDependency, held)
		return ctrl.Result{RequeueAfter: installRetry}, r.report(ctx, cp,
			simplyblockv1alpha2.ControlPlanePhaseInstalling, held)
	}

	next, ok := nextStep(machine)
	if !ok {
		// AwaitingAPI is terminal, and reaching it is what makes the control
		// plane Available. The step is left on the object as the record of how
		// the install finished.
		return r.steadyState(ctx, cp)
	}
	if err := machine.TransitionTo(ctx, next); err != nil {
		return ctrl.Result{}, fmt.Errorf("enter step %s: %w", next, err)
	}
	r.recordStepDuration(cp, current)
	snapshot := statemachine.ToKube(machine.Snapshot())
	return ctrl.Result{RequeueAfter: installAdvance}, r.recordStep(ctx, cp, next, snapshot.Deadline)
}

// performInstallStep does what one step is for, and reports whether the step is
// finished. A step that is not finished returns what it is waiting on, which is
// read from the thing being waited for so that a stalled install names the
// dependency rather than the operator.
func (r *ControlPlaneReconciler) performInstallStep(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane, current installStep,
) (done bool, held string, err error) {
	switch current {
	case stepApplyingFoundationDB:
		return true, "", applyAll(ctx, r.Client, cp, r.Scheme, foundationDBObjects(cp))

	case stepAwaitingFoundationDB:
		health, err := readFoundationDB(ctx, r.Client, cp.Namespace)
		if err != nil {
			return false, "", err
		}
		if waiting := health.waitingOn(); waiting != "" {
			return false, waiting, nil
		}
		return true, "", nil

	case stepApplyingDatastore:
		return true, "", applyAll(ctx, r.Client, cp, r.Scheme, datastoreObjects(cp))

	case stepApplyingAPI:
		return true, "", applyAll(ctx, r.Client, cp, r.Scheme, managementAPIObjects(cp))

	case stepAwaitingAPI:
		ok, message := r.probe(ctx, cp.Namespace, managedAccess{endpoint: localEndpoint(cp.Namespace)})
		if !ok {
			return false, fmt.Sprintf("the management API is not answering yet: %s", message), nil
		}
		return true, "", nil

	default:
		return false, "", fmt.Errorf("unknown installation step %q", current)
	}
}

// steadyState re-applies what the install created, probes readiness, and
// republishes the endpoint, the version, and the component counts.
//
// Re-applying is what keeps an object somebody deleted or edited from staying
// that way, and it is why no operation exists for checking the install (§6).
func (r *ControlPlaneReconciler) steadyState(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (ctrl.Result, error) {
	if err := r.applyEverything(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}

	// A managed control plane is reached on the Service this install created, so
	// there is no token to present and no CA beyond the cluster's own.
	access := managedAccess{endpoint: localEndpoint(cp.Namespace)}
	ok, message := r.probe(ctx, cp.Namespace, access)

	components, err := observe(ctx, r.Client, cp.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.recordComponents(cp.Namespace, components)

	phase, reason := derivePhase(ok, message, components)
	r.announce(cp, phase, reason)

	return ctrl.Result{RequeueAfter: steadyStateInterval}, r.publish(ctx, cp, statusUpdate{
		phase:      phase,
		message:    reason,
		endpoint:   access.endpoint,
		version:    r.version(ctx, access),
		components: components,
		probed:     true,
	})
}

// applyEverything writes every object of every step, which is the re-apply
// steady state performs. The order is the installation's, because the
// dependencies between the objects do not change once they exist.
func (r *ControlPlaneReconciler) applyEverything(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) error {
	for _, set := range [][]client.Object{
		foundationDBObjects(cp),
		datastoreObjects(cp),
		managementAPIObjects(cp),
	} {
		if err := applyAll(ctx, r.Client, cp, r.Scheme, set); err != nil {
			return err
		}
	}
	return nil
}

// probe performs the readiness read and records what it cost.
func (r *ControlPlaneReconciler) probe(
	ctx context.Context, namespace string, access managedAccess,
) (bool, string) {
	prober := r.Prober
	if prober == nil {
		prober = &HTTPProber{Token: access.token, Client: access.client}
	}

	started := time.Now()
	ok, message := prober.Ready(ctx, access.endpoint)
	controlPlaneProbeDuration.WithLabelValues(namespace).Observe(time.Since(started).Seconds())

	if ok {
		controlPlaneReadyState.WithLabelValues(namespace).Set(1)
	} else {
		controlPlaneReadyState.WithLabelValues(namespace).Set(0)
		controlPlaneProbeFailures.WithLabelValues(namespace, probeFailureReason(message)).Inc()
	}
	return ok, message
}

// version reads what the management API reports. A read that fails publishes
// nothing rather than clearing what was published, because a version the
// operator could not confirm this pass is not a version that changed.
func (r *ControlPlaneReconciler) version(ctx context.Context, access managedAccess) string {
	prober := r.Prober
	if prober == nil {
		prober = &HTTPProber{Token: access.token, Client: access.client}
	}
	version, err := prober.Version(ctx, access.endpoint)
	if err != nil {
		return ""
	}
	return version
}

// statusUpdate is what one pass concluded, applied to the object as a whole so
// that the phase and the evidence behind it are never written apart.
type statusUpdate struct {
	phase      simplyblockv1alpha2.ControlPlanePhase
	message    string
	endpoint   string
	version    string
	components []simplyblockv1alpha2.ControlPlaneComponentStatus

	// probed says whether this pass ran the readiness probe, which is what makes
	// status.lastChecked mean what it says.
	probed bool
}

// publish writes the conclusion of one pass.
func (r *ControlPlaneReconciler) publish(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane, update statusUpdate,
) error {
	return r.writeStatus(ctx, cp, func(status *simplyblockv1alpha2.ControlPlaneStatus) {
		status.Phase = update.phase
		status.Message = update.message
		status.Endpoint = update.endpoint
		status.Components = update.components
		if update.version != "" {
			status.Version = update.version
		}
		if update.probed {
			now := metav1.Now()
			status.LastChecked = &now
		}
	})
}

// report writes a phase and its reason and nothing else, which is what a pass
// that reached no conclusion about the endpoint or the components owes.
func (r *ControlPlaneReconciler) report(
	ctx context.Context,
	cp *simplyblockv1alpha2.ControlPlane,
	phase simplyblockv1alpha2.ControlPlanePhase,
	message string,
) error {
	return r.writeStatus(ctx, cp, func(status *simplyblockv1alpha2.ControlPlaneStatus) {
		status.Phase = phase
		status.Message = message
	})
}

// recordStep persists the step and its deadline before the side effect that step
// performs, which is the write-ahead record every multi-step operation in this
// group keeps.
func (r *ControlPlaneReconciler) recordStep(
	ctx context.Context,
	cp *simplyblockv1alpha2.ControlPlane,
	next installStep,
	stepDeadline *metav1.Time,
) error {
	return r.writeStatus(ctx, cp, func(status *simplyblockv1alpha2.ControlPlaneStatus) {
		status.Phase = simplyblockv1alpha2.ControlPlanePhaseInstalling
		status.Step = statemachine.KubeSnapshot{State: string(next), Deadline: stepDeadline}
	})
}

// enterStepAt sets a step's deadline from the graph's budget and persists it. It
// is what the first pass of an install does, and what a status carrying a step
// nothing recognizes is reset to.
func (r *ControlPlaneReconciler) enterStepAt(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane, step installStep,
) (ctrl.Result, error) {
	budget, ok := installStepBudgets[step]
	if !ok {
		budget = applyingFoundationDBDeadline
	}
	stepDeadline := metav1.NewTime(time.Now().Add(budget))
	return ctrl.Result{RequeueAfter: installAdvance}, r.recordStep(ctx, cp, step, &stepDeadline)
}

// nextStep is the step that follows the current one. The installation graph is a
// line, so the first edge is the only edge, and no edge means the step is
// terminal.
func nextStep(machine *statemachine.Machine[installStep]) (installStep, bool) {
	for next := range machine.AllowedTransitions() {
		return next, true
	}
	return machine.CurrentState(), false
}

// recordStepDuration measures how long a step took from the budget it was given
// and the deadline left on it. It is derived rather than timestamped because the
// deadline is already persisted and a second timestamp would be a field of the
// API that exists to be subtracted from another one.
func (r *ControlPlaneReconciler) recordStepDuration(
	cp *simplyblockv1alpha2.ControlPlane, step installStep,
) {
	budget, ok := installStepBudgets[step]
	if !ok || cp.Status.Step.Deadline == nil {
		return
	}
	remaining := time.Until(cp.Status.Step.Deadline.Time)
	spent := budget - remaining
	if spent < 0 {
		return
	}
	controlPlaneInstallStepDuration.
		WithLabelValues(cp.Namespace, string(step)).Observe(spent.Seconds())
}

// recordComponents publishes each component's two counts.
func (r *ControlPlaneReconciler) recordComponents(
	namespace string, components []simplyblockv1alpha2.ControlPlaneComponentStatus,
) {
	for _, component := range components {
		essential := "false"
		if component.Essential {
			essential = "true"
		}
		controlPlaneComponentReady.
			WithLabelValues(namespace, component.Name, essential).Set(float64(component.Ready))
		controlPlaneComponentDesired.
			WithLabelValues(namespace, component.Name, essential).Set(float64(component.Desired))
	}
}

// announce emits the phase's event, and only on a transition.
//
// A thirty-second probe that emitted on every failure would produce two thousand
// events a day from one outage, so what is worth an event is the arrival at a
// phase rather than the phase itself.
func (r *ControlPlaneReconciler) announce(
	cp *simplyblockv1alpha2.ControlPlane,
	phase simplyblockv1alpha2.ControlPlanePhase,
	message string,
) {
	if phase == cp.Status.Phase {
		return
	}
	switch phase {
	case simplyblockv1alpha2.ControlPlanePhaseUnavailable:
		r.emit(cp, corev1.EventTypeWarning, ControlPlaneNotReady, message)
	case simplyblockv1alpha2.ControlPlanePhaseDegraded:
		r.emit(cp, corev1.EventTypeWarning, ControlPlaneDegraded, message)
	case simplyblockv1alpha2.ControlPlanePhaseAvailable:
		r.emit(cp, corev1.EventTypeNormal, ControlPlaneReady,
			"the control plane's readiness probe passed")
	}
}

// finalize refuses while any StorageCluster in the namespace still exists, then
// removes what the garbage collector will not.
//
// The refusal is the point: deleting a ControlPlane with a managed source
// deletes a database, and the clusters, their UUIDs, and their volumes live in
// it. It is a hold rather than a failure — removing the clusters resolves it,
// and nothing else can.
//
// The same hold applies to a remote control plane, because a namespace whose
// clusters have no control plane to reach is a namespace of objects nothing can
// reconcile.
func (r *ControlPlaneReconciler) finalize(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cp, FinalizerControlPlane) {
		return ctrl.Result{}, nil
	}

	var clusters simplyblockv1alpha2.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(cp.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	if len(clusters.Items) > 0 {
		message := fmt.Sprintf(
			"%d StorageCluster objects still exist in %s, and their data lives behind this "+
				"control plane; remove them first",
			len(clusters.Items), cp.Namespace)
		r.emit(cp, corev1.EventTypeWarning, ClustersStillPresent, message)
		return ctrl.Result{RequeueAfter: steadyStateInterval}, r.report(ctx, cp, cp.Status.Phase, message)
	}

	// Deleting a control plane that never held the install must take nothing with
	// it. The check is repeated here rather than trusted from the reconcile that
	// added the finalizer, because an object may carry one from before this
	// ordering existed, and the cost of being wrong is the running deployment's
	// RBAC.
	holder, err := r.deploymentHolder(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	heldTheInstall := holder.Name == "" || holder == client.ObjectKeyFromObject(cp)

	// A remote control plane had nothing installed, so there is nothing
	// cluster-scoped to remove: deletion takes the object and touches neither
	// the endpoint nor its data.
	if isLocal(cp) && heldTheInstall {
		for _, obj := range append(foundationDBClusterScoped(), managementAPIClusterScoped()...) {
			if err := deleteIfMarked(ctx, r.Client, obj); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	base := cp.DeepCopy()
	controllerutil.RemoveFinalizer(cp, FinalizerControlPlane)
	return ctrl.Result{}, r.Patch(ctx, cp, client.MergeFrom(base))
}

func (r *ControlPlaneReconciler) ensureFinalizer(
	ctx context.Context, cp *simplyblockv1alpha2.ControlPlane,
) error {
	if controllerutil.ContainsFinalizer(cp, FinalizerControlPlane) {
		return nil
	}
	base := cp.DeepCopy()
	controllerutil.AddFinalizer(cp, FinalizerControlPlane)
	return r.Patch(ctx, cp, client.MergeFrom(base))
}

// deploymentHolder is the ControlPlane that owns the install: the oldest in the
// Kubernetes cluster, with namespace and name breaking a tie. Every reconciler
// picks the same one from the same list, so two of them never disagree about
// which object is the second.
func (r *ControlPlaneReconciler) deploymentHolder(ctx context.Context) (client.ObjectKey, error) {
	var list simplyblockv1alpha2.ControlPlaneList
	if err := r.List(ctx, &list); err != nil {
		return client.ObjectKey{}, err
	}

	var oldest *simplyblockv1alpha2.ControlPlane
	for i := range list.Items {
		item := &list.Items[i]
		if item.Name != SingletonName {
			continue
		}
		if oldest == nil || olderThan(item, oldest) {
			oldest = item
		}
	}
	if oldest == nil {
		return client.ObjectKey{}, nil
	}
	return client.ObjectKey{Namespace: oldest.Namespace, Name: oldest.Name}, nil
}

// olderThan orders two control planes by creation time, then by namespace and
// name. The tie-break matters because a creation timestamp has one-second
// resolution, so two objects applied together compare equal on it.
func olderThan(a, b *simplyblockv1alpha2.ControlPlane) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}

// foundationDBServed reports whether the API server knows the FoundationDB
// kinds. It asks the manager's RESTMapper rather than a discovery client,
// because the mapper is already backed by discovery and is what an apply would
// consult anyway.
func (r *ControlPlaneReconciler) foundationDBServed() (bool, error) {
	_, err := r.RESTMapper().RESTMapping(fdbClusterGVK.GroupKind(), fdbClusterGVK.Version)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, err
	}
}

// writeStatus applies a mutation to the live status, retrying a conflict. The
// observed generation is stamped here rather than by each caller, so that no
// path can write a phase without saying which spec it was computed from.
func (r *ControlPlaneReconciler) writeStatus(
	ctx context.Context,
	cp *simplyblockv1alpha2.ControlPlane,
	mutate func(*simplyblockv1alpha2.ControlPlaneStatus),
) error {
	key := client.ObjectKeyFromObject(cp)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current simplyblockv1alpha2.ControlPlane
		if err := r.Get(ctx, key, &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		mutate(&current.Status)
		current.Status.ObservedGeneration = current.Generation
		if err := r.Status().Update(ctx, &current); err != nil {
			return err
		}
		cp.Status = current.Status
		return nil
	})
}

// emit records what the reconcile decided, so that a refusal to act is visible
// to somebody reading the object rather than only in a log.
func (r *ControlPlaneReconciler) emit(
	object client.Object, eventType, reason, message string,
) {
	if r.Recorder == nil {
		return
	}
	// The action is the reason, as every other recorder call in this operator
	// passes it. It is a required field, and the phase would be empty on the
	// first event an object ever emits.
	r.Recorder.Eventf(object, nil, eventType, reason, reason, "%s", message)
}

// errorsAs is errors.As, wrapped so this file does not import the standard
// errors package alongside the Kubernetes one under a renamed identifier.
func errorsAs[T error](err error, target *T) bool {
	for err != nil {
		if typed, ok := err.(T); ok {
			*target = typed
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// SetupWithManager registers the reconciler.
//
// The workloads are watched as well as the ControlPlane itself, because a
// component's ready count changing is what moves the phase between Available and
// Degraded, and waiting out the steady-state interval to notice would make the
// phase lag an outage by half a minute.
//
// The ControlPlane's own watch is filtered to generation changes. Every probe
// stamps status.lastChecked, and an unfiltered watch turns that write into
// another reconcile, which probes and stamps again.
func (r *ControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.ControlPlane{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Named("controlplane").
		Complete(r)
}
