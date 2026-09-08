// The reconciler for OperatorOps, which today means one action: a discovery run
// that writes a ClusterDeploymentConfig.
//
// The run is three steps and each one persists what it concluded before moving
// on, because every one of them would otherwise be re-decided on the next
// reconcile against a cluster that had changed underneath it. Inspecting settles
// which workers the run is about, so a node joining mid-run does not silently
// join the draft. Probing creates one Job per worker and reads their reports.
// Writing turns the reports into a document and stops.
//
// Nothing here blocks. A step that is not finished requeues, and the step it is
// on is in the status, so a controller restart resumes rather than restarts.
//
// The run changes nothing it did not create. It reads nodes, creates probe Jobs
// and reads the ConfigMaps they write, and creates one ClusterDeploymentConfig
// in Draft. Whether that document becomes a cluster is the reviewer's decision
// and a different controller's job.

package deployment

import (
	"context"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/inventory"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	discoverypkg "github.com/simplyblock/simplyblock-operator/internal/discovery"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

const (
	// operatorOpsFinalizer holds a run in Terminating until it reaches a
	// terminal phase, so that a delete arriving mid-Probing cannot leave probe
	// Jobs running with nothing recording them.
	operatorOpsFinalizer = "storage.simplyblock.io/operatorops-finalizer"

	// probingRequeue is how often Probing looks again while Jobs are running.
	probingRequeue = 10 * time.Second

	// probingDeadline bounds the whole Probing step. A fleet's probes are
	// seconds of work each and run in parallel, so a step still waiting after
	// this has a Job that will not finish.
	probingDeadline = 15 * time.Minute

	// discoveryNodeSetSuffix and clusterNameSuffix name what a run produces
	// when the caller named nothing.
	configNamePrefix  = "discovered-"
	clusterNameSuffix = "-cluster"
)

// OperatorOpsReconciler runs operations against the operator itself.
type OperatorOpsReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Discovery lists the API groups the server registers, which is the
	// strongest evidence of which distribution installed the kubelet. It is
	// optional: a run without it concludes the environment from the nodes
	// alone.
	Discovery discovery.DiscoveryInterface

	// ProbeImage is the image the probe Jobs run, which is the operator's own:
	// the probe binary ships in it beside the manager, so a Job cannot be a
	// version out of step with the operator that created it.
	ProbeImage string

	// ProbeServiceAccount is the account the probe writes its report as. It
	// needs create and update on ConfigMaps in the operator's namespace and
	// nothing else. Empty is DefaultNodeProbeServiceAccount, which is what the
	// chart creates.
	ProbeServiceAccount string
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=operatorops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=operatorops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=operatorops/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=clusterdeploymentconfigs,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=clusterdeploymentconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storagenodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile advances one operator operation by one step.
func (r *OperatorOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ops simplyblockv1alpha2.OperatorOps
	if err := r.Get(ctx, req.NamespacedName, &ops); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ops.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &ops)
	}
	if !slices.Contains(ops.Finalizers, operatorOpsFinalizer) {
		ops.Finalizers = append(ops.Finalizers, operatorOpsFinalizer)
		return ctrl.Result{}, r.Update(ctx, &ops)
	}

	// Terminal and staying that way: the run is the audit record now.
	switch ops.Status.Phase {
	case simplyblockv1alpha2.OperatorOpsPhaseSucceeded,
		simplyblockv1alpha2.OperatorOpsPhaseFailed,
		simplyblockv1alpha2.OperatorOpsPhaseAborted:
		return ctrl.Result{}, nil
	}

	if ops.Spec.Abort {
		return r.abort(ctx, &ops)
	}

	if ops.Spec.Action != simplyblockv1alpha2.OperatorOpsActionDiscover {
		// The enum admits nothing else, so this is the schema having been
		// bypassed rather than a case to handle.
		return r.fail(ctx, &ops, fmt.Sprintf("action %q is not one this operator runs", ops.Spec.Action))
	}

	if r.ProbeImage == "" {
		// Checked here rather than at startup, because an operator with no
		// probe image is fine until somebody asks it to discover, and failing
		// the run says so where a failed startup would not.
		return r.fail(ctx, &ops, NodeProbeImageError().Error())
	}

	log.Info("advancing discovery", "step", ops.Status.Step.State, "phase", ops.Status.Phase)

	switch simplyblockv1alpha2.OperatorOpsStep(ops.Status.Step.State) {
	case "":
		return r.startInspecting(ctx, &ops)
	case simplyblockv1alpha2.OperatorOpsStepInspecting:
		return r.inspect(ctx, &ops)
	case simplyblockv1alpha2.OperatorOpsStepProbing:
		return r.probe(ctx, &ops)
	case simplyblockv1alpha2.OperatorOpsStepWriting:
		return r.write(ctx, &ops)
	default:
		return r.fail(ctx, &ops, fmt.Sprintf("step %q is not one this action has", ops.Status.Step.State))
	}
}

// startInspecting records that the run has begun before it does anything, so
// that a crash between the two is visible as a run that started rather than one
// that never did.
func (r *OperatorOpsReconciler) startInspecting(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
) (ctrl.Result, error) {
	now := metav1.Now()
	ops.Status.Phase = simplyblockv1alpha2.OperatorOpsPhaseRunning
	ops.Status.StartedAt = &now
	ops.Status.Step.State = string(simplyblockv1alpha2.OperatorOpsStepInspecting)
	ops.Status.Message = "reading the cluster's workers"
	r.event(ops, corev1.EventTypeNormal, "OperationStarted", "discovery started")

	return ctrl.Result{Requeue: true}, r.status(ctx, ops)
}

// inspect settles what the run is about: which workers, and which distribution.
//
// It is one step and it persists both, because everything after it has to agree
// with what it decided. A worker that joins the cluster while the probes are
// running is not in this run's draft, and re-listing in Probing would put it
// there with no probe report to describe it.
func (r *OperatorOpsReconciler) inspect(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
) (ctrl.Result, error) {
	spec := ops.Spec.Discover
	if spec == nil {
		spec = &simplyblockv1alpha2.DiscoverSpec{}
	}

	var nodes corev1.NodeList
	options := []client.ListOption{}
	if len(spec.NodeSelector) > 0 {
		options = append(options, client.MatchingLabels(spec.NodeSelector))
	}
	if err := r.List(ctx, &nodes, options...); err != nil {
		return ctrl.Result{}, err
	}

	taken, err := r.workersAlreadyTaken(ctx, ops.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	workers := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if !schedulable(node) {
			continue
		}
		if _, already := taken[node.Name]; already {
			continue
		}
		workers = append(workers, node.Name)
	}
	slices.Sort(workers)

	// The environment is the cluster's, so it is concluded once here from the
	// whole node list rather than by each probe.
	environment, err := inventory.CollectEnvironment(ctx, r.Discovery, nodes.Items)
	if err != nil {
		// Half the evidence still yields a conclusion, and the field is one a
		// reviewer corrects, so this is recorded and not fatal.
		r.event(ops, corev1.EventTypeWarning, "EnvironmentPartiallyRead",
			fmt.Sprintf("the API groups could not be listed, so the distribution was concluded from the nodes alone: %v", err))
	}

	if len(workers) == 0 {
		return r.fail(ctx, ops,
			"no schedulable worker is free: every node either carries a StorageNode already, "+
				"is unschedulable, or does not match the run's selector")
	}

	ops.Status.Workers = workers
	ops.Status.Environment = simplyblockv1alpha2.KubernetesEnvironment(environment.Distribution)
	ops.Status.Step.State = string(simplyblockv1alpha2.OperatorOpsStepProbing)
	deadline := metav1.NewTime(time.Now().Add(probingDeadline))
	ops.Status.Step.Deadline = &deadline
	ops.Status.Message = fmt.Sprintf("probing %d worker(s) of a %s cluster",
		len(workers), orUnknown(string(environment.Distribution)))

	return ctrl.Result{Requeue: true}, r.status(ctx, ops)
}

// workersAlreadyTaken is the set of workers a StorageNode already runs on.
//
// A run reports only what is unclaimed, which is what makes re-running it
// useful: a run against a deployed fleet finds the machines nobody has taken
// yet, and what it writes is a growth document rather than a description of the
// cluster that exists.
func (r *OperatorOpsReconciler) workersAlreadyTaken(
	ctx context.Context,
	namespace string,
) (map[string]struct{}, error) {
	var nodes simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	taken := make(map[string]struct{}, len(nodes.Items))
	for _, node := range nodes.Items {
		if node.Spec.WorkerNode != "" {
			taken[node.Spec.WorkerNode] = struct{}{}
		}
	}
	return taken, nil
}

// probe creates the missing probe Jobs and waits for their reports.
//
// It is idempotent by construction: a Job's name is derived from the run and
// the worker, so a reconcile that has already created one finds it rather than
// creating a second.
func (r *OperatorOpsReconciler) probe(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if deadline, has := ops.Status.Step.KubeDeadline(); has && time.Now().After(deadline) {
		return r.fail(ctx, ops, fmt.Sprintf(
			"the probes did not all finish within %s; the reports that did arrive are in the "+
				"ConfigMaps labelled for this run", probingDeadline))
	}

	owner := metav1.NewControllerRef(ops,
		simplyblockv1alpha2.GroupVersion.WithKind("OperatorOps"))

	reports, err := r.reportsFor(ctx, ops)
	if err != nil {
		return ctrl.Result{}, err
	}

	waiting, failed := 0, []string{}
	for _, worker := range ops.Status.Workers {
		if _, arrived := reports[worker]; arrived {
			continue
		}

		job, err := nodeprobe.Job(nodeprobe.JobOptions{
			Namespace:          ops.Namespace,
			Run:                ops.Name,
			Node:               worker,
			Image:              r.ProbeImage,
			ServiceAccountName: r.probeServiceAccount(),
			Owner:              owner,
		})
		if err != nil {
			return r.fail(ctx, ops, fmt.Sprintf("a probe Job could not be built: %v", err))
		}

		var existing batchv1.Job
		switch err := r.Get(ctx, client.ObjectKeyFromObject(job), &existing); {
		case apierrors.IsNotFound(err):
			if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
			log.Info("created a probe Job", "worker", worker, "job", job.Name)
			waiting++
		case err != nil:
			return ctrl.Result{}, err
		case existing.Status.Failed > 0 && jobExhausted(&existing):
			// A Job that has used its retries and written no report is a
			// worker this run cannot describe. It does not fail the run: the
			// rest of the fleet is still worth a draft, and the worker is
			// named so a reader knows what is missing.
			failed = append(failed, worker)
		default:
			waiting++
		}
	}

	if waiting > 0 {
		ops.Status.Message = fmt.Sprintf("%d of %d worker(s) reported; waiting for %d",
			len(reports), len(ops.Status.Workers), waiting)
		if err := r.status(ctx, ops); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: probingRequeue}, nil
	}

	if len(reports) == 0 {
		return r.fail(ctx, ops, fmt.Sprintf(
			"no worker reported: every one of the %d probe Jobs failed", len(ops.Status.Workers)))
	}
	for _, worker := range failed {
		r.event(ops, corev1.EventTypeWarning, "DeviceInspectionFailed",
			fmt.Sprintf("the probe on %s failed, so it is not in the draft", worker))
	}

	ops.Status.Step.State = string(simplyblockv1alpha2.OperatorOpsStepWriting)
	ops.Status.Step.Deadline = nil
	ops.Status.Message = fmt.Sprintf("%d worker(s) reported; writing the draft", len(reports))
	return ctrl.Result{Requeue: true}, r.status(ctx, ops)
}

// write turns the reports into a ClusterDeploymentConfig in Draft.
func (r *OperatorOpsReconciler) write(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
) (ctrl.Result, error) {
	spec := ops.Spec.Discover
	if spec == nil {
		spec = &simplyblockv1alpha2.DiscoverSpec{}
	}

	reports, err := r.reportsFor(ctx, ops)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(reports) == 0 {
		return r.fail(ctx, ops, "the probe reports are gone, so there is nothing to write")
	}

	collected := make([]nodeprobe.Report, 0, len(reports))
	for _, report := range reports {
		collected = append(collected, report)
	}

	filter := spec.DeviceFilter
	planner := discoverypkg.Planner{Class: discoverypkg.ClassOf(filter)}
	plan := planner.Plan(collected, filter)

	if len(plan.NodeSets) == 0 {
		return r.fail(ctx, ops, fmt.Sprintf(
			"no worker has a device this run would use: %s", plan.Summary()))
	}

	config, notes := r.draftFor(ops, spec, plan)
	if err := r.Create(ctx, config); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		// A run whose name was reused, or one that crashed after creating the
		// document and before recording it. The document is what matters and
		// it exists, so the run adopts it rather than failing.
		r.event(ops, corev1.EventTypeNormal, "ConfigExists",
			fmt.Sprintf("%s already existed and was left as it is", config.Name))
	}

	for _, note := range notes {
		r.event(ops, corev1.EventTypeNormal, "ConfigWritten", note)
	}
	r.event(ops, corev1.EventTypeNormal, "ConfigWritten",
		fmt.Sprintf("wrote %s in Draft: %s", config.Name, plan.Summary()))
	for _, refusal := range plan.RefusalLines() {
		r.event(ops, corev1.EventTypeNormal, "DeviceDeclined", refusal)
	}

	now := metav1.Now()
	ops.Status.ConfigRef = config.Name
	ops.Status.Phase = simplyblockv1alpha2.OperatorOpsPhaseSucceeded
	ops.Status.CompletedAt = &now
	ops.Status.Step.Deadline = nil
	ops.Status.Message = fmt.Sprintf("wrote %s awaiting approval: %s", config.Name, plan.Summary())
	r.event(ops, corev1.EventTypeNormal, "OperationSucceeded", ops.Status.Message)

	return ctrl.Result{}, r.status(ctx, ops)
}

// draftFor builds the document, and the notes explaining the numbers in it that
// were not read off the hardware.
func (r *OperatorOpsReconciler) draftFor(
	ops *simplyblockv1alpha2.OperatorOps,
	spec *simplyblockv1alpha2.DiscoverSpec,
	plan discoverypkg.Plan,
) (*simplyblockv1alpha2.ClusterDeploymentConfig, []string) {
	name := spec.ConfigName
	if name == "" {
		name = configNamePrefix + ops.Name
	}

	config := &simplyblockv1alpha2.ClusterDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ops.Namespace,
			Labels: map[string]string{
				nodeprobe.LabelComponent: nodeprobe.ComponentNodeProbe,
				nodeprobe.LabelRun:       ops.Name,
			},
		},
		Spec: simplyblockv1alpha2.ClusterDeploymentConfigSpec{
			// Always false, and never anything else, including on a re-run. A
			// second discovery writes a second document rather than editing
			// the first, because the first may have been reviewed and
			// overwriting a reviewer's corrections with a fresh guess is the
			// worst behavior available.
			Approved:    false,
			Environment: ops.Status.Environment,
			NodeSets:    plan.NodeSets,
		},
	}

	var notes []string
	if spec.ClusterRef != "" {
		// A growth document: the cluster's layout is settled, and naming a
		// template beside a reference is what admission refuses.
		config.Spec.ClusterRef = spec.ClusterRef
		notes = append(notes, fmt.Sprintf(
			"the draft grows the existing cluster %s, so it proposes no cluster layout", spec.ClusterRef))
	} else {
		template := discoverypkg.ClusterTemplateFor(name+clusterNameSuffix, plan)
		config.Spec.Cluster = template.Template
		notes = append(notes, template.Notes...)
	}
	return config, notes
}

// reportsFor reads the reports this run's probes have written, keyed by worker.
//
// A ConfigMap whose report cannot be parsed is skipped with an event rather
// than failing the run: the worker it describes is left out of the draft, which
// is the same outcome as a probe that never finished, and the rest of the fleet
// is still worth a document.
func (r *OperatorOpsReconciler) reportsFor(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
) (map[string]nodeprobe.Report, error) {
	var maps corev1.ConfigMapList
	if err := r.List(ctx, &maps,
		client.InNamespace(ops.Namespace),
		client.MatchingLabels(nodeprobe.ReportSelector(ops.Name)),
	); err != nil {
		return nil, err
	}

	reports := make(map[string]nodeprobe.Report, len(maps.Items))
	for i := range maps.Items {
		report, err := nodeprobe.ReportFromConfigMap(&maps.Items[i])
		if err != nil {
			r.event(ops, corev1.EventTypeWarning, "ReportUnreadable", err.Error())
			continue
		}
		if !slices.Contains(ops.Status.Workers, report.Node) {
			// A report for a worker this run is not about, which is what a
			// reused run name produces. It is not this run's evidence.
			continue
		}
		reports[report.Node] = report
	}
	return reports, nil
}

// probeServiceAccount is the account the probes run as, defaulted to the one
// the chart creates.
func (r *OperatorOpsReconciler) probeServiceAccount() string {
	if r.ProbeServiceAccount != "" {
		return r.ProbeServiceAccount
	}
	return nodeProbeServiceAccount()
}

// abort stops a run at its next step. Discovery changes nothing, so there is
// nothing to unwind beyond the Jobs it started.
func (r *OperatorOpsReconciler) abort(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
) (ctrl.Result, error) {
	now := metav1.Now()
	ops.Status.Phase = simplyblockv1alpha2.OperatorOpsPhaseAborted
	ops.Status.CompletedAt = &now
	ops.Status.Step.Deadline = nil
	ops.Status.Message = "aborted; discovery changes nothing, so nothing was undone"
	r.event(ops, corev1.EventTypeNormal, "OperationAborted", ops.Status.Message)
	return ctrl.Result{}, r.status(ctx, ops)
}

// finalize lets a deleted run go once it is terminal, and aborts it first when
// it is not.
//
// The Jobs and the reports are collected by the garbage collector, because both
// carry an owner reference to the run. What the finalizer is for is the window
// in between: a delete arriving mid-Probing would otherwise leave the Jobs
// running with nothing recording what they found.
func (r *OperatorOpsReconciler) finalize(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
) (ctrl.Result, error) {
	terminal := ops.Status.Phase == simplyblockv1alpha2.OperatorOpsPhaseSucceeded ||
		ops.Status.Phase == simplyblockv1alpha2.OperatorOpsPhaseFailed ||
		ops.Status.Phase == simplyblockv1alpha2.OperatorOpsPhaseAborted

	if !terminal && ops.Status.Phase != "" {
		if _, err := r.abort(ctx, ops); err != nil {
			return ctrl.Result{}, err
		}
	}

	ops.Finalizers = slices.DeleteFunc(ops.Finalizers, func(f string) bool {
		return f == operatorOpsFinalizer
	})
	return ctrl.Result{}, r.Update(ctx, ops)
}

// fail ends a run with a reason.
func (r *OperatorOpsReconciler) fail(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
	reason string,
) (ctrl.Result, error) {
	now := metav1.Now()
	ops.Status.Phase = simplyblockv1alpha2.OperatorOpsPhaseFailed
	ops.Status.CompletedAt = &now
	ops.Status.Step.Deadline = nil
	ops.Status.Message = reason
	r.event(ops, corev1.EventTypeWarning, "OperationFailed", reason)
	return ctrl.Result{}, r.status(ctx, ops)
}

// status writes the run's status, stamping the generation it was computed from
// so that a stale status can be told from a current one.
func (r *OperatorOpsReconciler) status(
	ctx context.Context,
	ops *simplyblockv1alpha2.OperatorOps,
) error {
	ops.Status.ObservedGeneration = ops.Generation
	return r.Status().Update(ctx, ops)
}

// event records one, when there is a recorder to record it with.
//
// The action is the step the run is on, so that a reader scanning a run's
// events sees which of the three produced each one.
func (r *OperatorOpsReconciler) event(
	ops *simplyblockv1alpha2.OperatorOps,
	eventType, reason, message string,
) {
	if r.Recorder == nil {
		return
	}
	action := ops.Status.Step.State
	if action == "" {
		action = string(ops.Spec.Action)
	}
	r.Recorder.Eventf(ops, nil, eventType, reason, action, "%s", message)
}

// schedulable reports whether a node is one work can be placed on.
//
// A cordoned node and a node carrying a NoSchedule taint are both excluded: a
// probe Job is pinned with spec.nodeName and would run on either, and a worker
// the cluster is not scheduling to is not one to hand to a storage cluster.
func schedulable(node corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return false
		}
	}
	return true
}

// jobExhausted reports whether a Job has used up its retries.
func jobExhausted(job *batchv1.Job) bool {
	limit := int32(1)
	if job.Spec.BackoffLimit != nil {
		limit = *job.Spec.BackoffLimit
	}
	if job.Status.Failed > limit {
		return true
	}
	return slices.ContainsFunc(job.Status.Conditions, func(c batchv1.JobCondition) bool {
		return c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue
	})
}

// orUnknown names a distribution for a message, including the one nothing
// concluded.
func orUnknown(distribution string) string {
	if distribution == "" {
		return "Kubernetes"
	}
	return distribution
}

// SetupWithManager registers the reconciler and the objects it owns, so that a
// probe Job finishing wakes the run that created it rather than waiting for the
// next poll.
func (r *OperatorOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha2.OperatorOps{}).
		Owns(&batchv1.Job{}).
		Named("operatorops").
		Complete(r)
}
