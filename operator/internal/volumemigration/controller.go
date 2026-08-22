package volumemigration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch

// Reconciler reconciles VolumeMigration resources.
type Reconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   events.EventRecorder
	apiClient  *webapi.Client
	coreClient corev1client.CoreV1Interface
	// apiReader is an uncached reader (mgr.GetAPIReader) used for the
	// "is this volume actively consumed?" decision. A stale informer cache could
	// otherwise miss a genuinely-running consumer and cause validation to be
	// skipped for a live volume, breaking its I/O path after cutover.
	apiReader client.Reader
}

// migrationDeferredRetryDelay is how long to wait before re-submitting a migration
// the control plane deferred (webapi.ErrMigrationNotAcceptingYet). Long enough not to
// hammer the API while a cluster-wide realignment runs, short enough that the
// migration starts promptly once it finishes.
const migrationDeferredRetryDelay = 30 * time.Second

// maxMigrationDeferral bounds the retrying: a cluster that has not accepted the
// migration within this window is not merely busy, and the migration fails with the
// control plane's own reason.
//
// Without a bound, a condition that never clears — say a rebalancing task whose runner
// is not running, which keeps _can_add_lvol_migration false indefinitely — would leave
// the migration retrying forever in a non-terminal phase. Everything that waits on a
// VolumeMigration (the rebalancer's tracking, StorageNodeOps, the PVC controller) waits
// for a terminal phase, so such a migration would stall those flows silently instead of
// reporting a failure they can act on.
const maxMigrationDeferral = 10 * time.Minute

// Validation of the NVMe paths has to finish inside the window the control plane keeps
// a created-but-unstarted migration open, so the whole Validating phase is bounded:
// at most maxConsumerWait waiting for consumer pods, then at most
// validationJobDeadline per Job (Jobs run in parallel, so that is not additive).
const (
	// maxConsumerWait bounds how long we wait for every consumer pod of the
	// subsystem to be Running. Past it the migration is cancelled rather than
	// continued with an unvalidated node — a consumer that starts mid-migration
	// stages against the source and is stranded at cutover.
	maxConsumerWait = 60 * time.Second

	// consumerWaitRetryDelay is how often to re-check while waiting.
	consumerWaitRetryDelay = 5 * time.Second

	// validationJobDeadline caps a validation Job's total lifetime, scheduling and
	// image pull included (activeDeadlineSeconds). Its purpose is to turn a Job that
	// can never finish — an unschedulable pod, a NotReady node — into a failure
	// instead of a migration parked in Validating forever. The validation itself
	// needs seconds: three connect+list attempts, two seconds apart.
	validationJobDeadline = 180 * time.Second
)

// errConsumerNotReady indicates that a pod references the volume's PVC but is not
// Running yet (e.g. Pending or scheduling). A consumer is coming, so validation
// must NOT be skipped: the caller should wait and validate on the consumer's node
// once it is Running, rather than continuing the migration unvalidated.
var errConsumerNotReady = errors.New("volume has a consumer that is not running yet")

// Reconcile drives one migration one step. The lifecycle itself lives in
// [Reconciler.machineFor]: this restores the machine to where the
// resource says the migration is, lets the current phase act, and writes the
// machine's new position back.
//
// The machine does not survive between reconciles — the process holds no memory of
// the last pass and may not even be the same process — so status.phase and
// status.phaseDeadline are where it lives, and every pass rebuilds it from them.
func (r *Reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	vm := &simplyblockv1alpha1.VolumeMigration{}
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	p := &migrationPass{vm: vm, before: vm.DeepCopy()}
	sm := r.machineFor(ctx, p)
	defer sm.Close()

	if err := r.restorePhase(ctx, sm, vm); err != nil {
		// An unrecognised phase: a downgrade, or a hand-edited resource. Retrying is
		// the only safe answer — guessing which phase was meant could re-submit a
		// migration that is already running.
		return ctrl.Result{}, err
	}

	// Completed, Failed and Aborted have no outgoing edges: nothing left to do.
	if sm.IsTerminal() {
		return ctrl.Result{}, nil
	}

	res, advanceErr := r.advance(ctx, p, sm)

	// The status write happens even when the step failed. A step that gave up halfway
	// may still have learned something worth keeping — a validation that passed on one
	// node, a deferral that started — and dropping it would mean re-running the work
	// that produced it, which for a validation means re-connecting NVMe paths.
	if err := r.persist(ctx, p, sm); err != nil {
		return ctrl.Result{}, errors.Join(advanceErr, err)
	}
	if advanceErr != nil {
		return ctrl.Result{}, advanceErr
	}
	return requeueWithin(res, sm), nil
}

// advance lets the current phase act, which is not the same as taking a transition:
// most passes of a Validating or Running migration only poll what they are waiting
// for and update counters, and the machine moves when that wait is over.
func (r *Reconciler) advance(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
) (ctrl.Result, error) {
	// An abort applies to any migration that has not finished, including one still
	// pending — a migration deferred by a busy cluster would otherwise keep retrying
	// for the whole deferral window while the user has already asked it to stop.
	if p.vm.Spec.Abort {
		return r.abort(ctx, p, sm)
	}

	switch sm.CurrentState() {
	case phaseValidating:
		return r.validateMigration(ctx, p, sm)
	case phaseRunning:
		return r.pollMigration(ctx, p, sm)
	default:
		return r.submitMigration(ctx, p, sm)
	}
}

// submitMigration resolves the PV to a logical volume, finds its cluster, pool and
// NVMe subsystem, and submits the migration to the storage API. It is the Pending
// phase's work: everything here happens before the control plane knows the migration
// exists, so anything malformed fails the migration outright rather than leaving
// something behind.
func (r *Reconciler) submitMigration(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
) (ctrl.Result, error) {
	vm := p.vm

	// Resolve PV → volume UUID via CSI volume handle.
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: vm.Spec.PVName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, p, sm, fmt.Sprintf("PersistentVolume %q not found", vm.Spec.PVName))
		}
		return ctrl.Result{}, fmt.Errorf("get PV %q: %w", vm.Spec.PVName, err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return r.fail(ctx, p, sm, fmt.Sprintf("PV %q has no CSI volume handle", vm.Spec.PVName))
	}
	// CSI volume handle format: "<clusterUUID>:<poolUUID>:<volumeUUID>"
	parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return r.fail(ctx, p, sm, fmt.Sprintf("PV %q has unexpected CSI volume handle format %q (expected <clusterUUID>:<poolUUID>:<volumeUUID>)", vm.Spec.PVName, pv.Spec.CSI.VolumeHandle))
	}
	// Recorded before the migration is submitted, so that a migration still being
	// retried already says which cluster it is waiting on.
	vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID = parts[0], parts[1], parts[2]

	if _, err := r.resolveRebalancerImage(ctx, vm.Namespace, vm.Status.ClusterUUID); err != nil {
		return r.fail(ctx, p, sm, fmt.Sprintf("volume migration not enabled/configured for cluster %q: %v", vm.Status.ClusterUUID, err))
	}

	// The storage API migrates a whole NVMe subsystem, addressed by its NQN, so
	// resolve the volume to its subsystem before submitting. For a namespaced
	// volume the subsystem is shared and its sibling volumes move along.
	volume, err := r.apiClient.GetVolume(ctx, vm.Status.ClusterUUID, vm.Status.PoolUUID, vm.Status.VolumeUUID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get volume %q: %w", vm.Status.VolumeUUID, err)
	}
	if volume == nil {
		return r.fail(ctx, p, sm, fmt.Sprintf("volume %s no longer exists", vm.Status.VolumeUUID))
	}
	if volume.NQN == "" {
		return r.fail(ctx, p, sm, fmt.Sprintf("volume %s has no subsystem NQN; cannot address its migration", vm.Status.VolumeUUID))
	}

	migration, err := r.apiClient.CreateMigration(ctx, vm.Status.ClusterUUID, volume.NQN, vm.Spec.TargetNodeUUID)
	switch {
	case errors.Is(err, webapi.ErrMigrationNotAcceptingYet):
		return r.deferMigration(ctx, p, sm, err)
	case isIndeterminateCreate(err):
		// The request never got an answer, so whether the control plane created the
		// migration is unknown — and it may well have: a create can take longer than the
		// client timeout while a rebalance is in flight, and it allocates bdevs on the way.
		// Failing here would abandon that half-created migration. Retrying instead lets the
		// next attempt hit the existing-migration path, which cancels it before re-creating.
		return r.retryIndeterminateCreate(ctx, p, sm, err)
	case err != nil:
		return r.fail(ctx, p, sm, fmt.Sprintf("CreateMigration: %v", err))
	}
	if migration == nil {
		// Previous migration had to be canceled, retry in the next reconcile cycle.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if migration.ID == "" {
		return r.fail(ctx, p, sm, "CreateMigration returned empty migration UUID")
	}

	// Accepted. The hook records what came back; it runs only if the phase is
	// actually entered, so there is no route that writes a Validating migration
	// without one behind it.
	p.created = migration
	if err := sm.TransitionTo(ctx, phaseValidating); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// holdInPending keeps a migration nothing has been submitted for in Pending and has
// it retried, failing it with reason once the window for that runs out.
//
// The window is armed by re-entering Pending, which happens exactly once — on the
// first pass that finds no deadline. Re-arming on every refusal would push the
// deadline out each time and the retrying would never end.
func (r *Reconciler) holdInPending(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
	reason string,
) (ctrl.Result, error) {
	if sm.TimeoutReached() {
		return r.fail(ctx, p, sm, reason)
	}
	if _, armed := sm.Deadline(); !armed {
		if err := sm.TransitionTo(ctx, phasePending); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: migrationDeferredRetryDelay}, nil
}

// deferredFor is how long the migration has been waiting to be accepted, for the log
// line and the event that report it. Zero if nothing recorded the start of the wait,
// which a hand-written status.phase can arrange.
func deferredFor(vm *simplyblockv1alpha1.VolumeMigration) time.Duration {
	if vm.Status.DeferredSince == nil {
		return 0
	}
	return time.Since(vm.Status.DeferredSince.Time).Round(time.Second)
}

// deferMigration holds a migration the control plane refused because the cluster is
// busy with work that ends on its own — typically the data realignment a previous
// migration triggered, which the control plane will not migrate through.
//
// The wait is bounded by the Pending phase's deadline, and past it the migration
// fails with the control plane's own reason. Reporting that reason is why the
// deadline is consulted here, holding a fresh refusal, rather than generically before
// the phase acts: "the cluster did not accept this in 10 minutes" is only actionable
// together with what it was refusing for.
func (r *Reconciler) deferMigration(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
	cause error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	vm := p.vm

	res, err := r.holdInPending(ctx, p, sm, fmt.Sprintf(
		"cluster %s did not accept the migration within %s: %v",
		vm.Status.ClusterUUID, maxMigrationDeferral, cause))
	if err != nil || sm.IsTerminal() {
		return res, err
	}

	waited := deferredFor(vm)
	log.Info("Cluster is not accepting migrations yet; retrying",
		"cluster", vm.Status.ClusterUUID, "waited", waited, "giveUpAfter", maxMigrationDeferral,
		"reason", cause.Error())
	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationDeferred", "MigrationDeferred",
		"Cluster %s is not accepting migrations yet (waiting %s of at most %s); retrying in %s",
		vm.Status.ClusterUUID, waited, maxMigrationDeferral, migrationDeferredRetryDelay)
	return res, nil
}

// isIndeterminateCreate reports whether a CreateMigration error left the outcome unknown
// rather than establishing that no migration was created. A timeout or a dropped
// connection says nothing about what the control plane did with the request; an HTTP
// status does, and is handled as a real failure.
func isIndeterminateCreate(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// retryIndeterminateCreate keeps a migration whose create timed out in Pending and retries
// it, bounded by the same window: the request may have taken effect, so the CR must not be
// failed until a later attempt can observe (and cancel) what was left behind.
func (r *Reconciler) retryIndeterminateCreate(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
	cause error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	vm := p.vm

	res, err := r.holdInPending(ctx, p, sm, fmt.Sprintf(
		"CreateMigration did not return within %s of retrying on cluster %s: %v",
		maxMigrationDeferral, vm.Status.ClusterUUID, cause))
	if err != nil || sm.IsTerminal() {
		return res, err
	}

	waited := deferredFor(vm)
	log.Info("CreateMigration gave no answer; the migration may exist on the backend, retrying",
		"cluster", vm.Status.ClusterUUID, "waited", waited, "giveUpAfter", maxMigrationDeferral,
		"reason", cause.Error())
	r.Recorder.Eventf(vm, nil, corev1.EventTypeWarning, "MigrationCreateIndeterminate",
		"MigrationCreateIndeterminate",
		"CreateMigration on cluster %s returned no answer (%v); it may have taken effect, "+
			"so retrying in %s rather than failing", vm.Status.ClusterUUID, cause, migrationDeferredRetryDelay)
	return res, nil
}

// validateMigration creates one Job per worker node that consumes a volume of the
// migrated subsystem. Each Job:
//  1. Checks whether this node has a host connection to the subsystem at all.
//  2. If it does, runs `nvme connect` for each connection returned by CreateMigration.
//  3. Runs `nvme list --verbose` and verifies all new NQNs appear with ANA
//     state "inaccessible" (paths connected but volume not yet migrated).
//
// Once every Job has succeeded the migration cuts over to Running. Any Job failing
// cancels it: cutover is subsystem-wide, so continuing with a subset of the consumers
// ready guarantees an outage for the rest.
func (r *Reconciler) validateMigration(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	vm := p.vm

	if vm.Status.MigrationUUID == "" {
		return r.fail(ctx, p, sm, "migration UUID is empty in Validating phase; status was likely written before a failed CreateMigration")
	}

	// The phase's bound is the outer limit on the whole validation. Reaching it means
	// something is not going to finish — a Job that cannot be scheduled and is not
	// being replaced, a node set that keeps growing — and the control plane will not
	// hold a created-but-unstarted migration open indefinitely anyway.
	if sm.TimeoutReached() {
		return r.cancelAndFail(ctx, p, sm, fmt.Sprintf(
			"NVMe path validation of subsystem %s did not finish within %s",
			vm.Status.SubsystemNQN, phaseBound(phaseValidating)))
	}

	// Jobs already created — poll them.
	if len(vm.Status.ValidationJobs) > 0 {
		return r.pollValidationJobs(ctx, p, sm)
	}

	nodes, err := r.resolveValidationNodes(ctx, vm)
	switch {
	case errors.Is(err, errConsumerNotReady):
		// A consumer pod exists but is not Running yet. Do not skip validation: wait
		// so its node gets the new paths too.
		if !mayWaitForConsumers(sm) {
			return r.cancelAndFail(ctx, p, sm, fmt.Sprintf(
				"consumer pods of subsystem %s were not all Running within %s: %v",
				vm.Status.SubsystemNQN, maxConsumerWait, err))
		}
		log.Info("Consumer pod not Running yet; waiting before NVMe path validation",
			"migration", vm.Status.MigrationUUID, "reason", err.Error())
		r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "WaitingForConsumer", "WaitingForConsumer",
			"Waiting for all consumers of subsystem %s to be Running before validation: %v",
			vm.Status.SubsystemNQN, err)
		return ctrl.Result{RequeueAfter: consumerWaitRetryDelay}, nil
	case err != nil:
		// A genuine lookup error (control-plane call, PV get, pod list) — retry later.
		log.Error(err, "Cannot resolve the nodes to validate; requeuing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	case len(nodes) == 0:
		// No volume of the subsystem has a consumer, so there are no NVMe I/O paths to
		// validate anywhere. Skip validation and cut over directly.
		log.Info("No consumer for any volume of the subsystem; skipping NVMe path validation",
			"subsystem", vm.Status.SubsystemNQN, "migration", vm.Status.MigrationUUID)
		r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "ValidationSkipped", "ValidationSkipped",
			"No consumer for any volume of subsystem %s; skipping NVMe path validation",
			vm.Status.SubsystemNQN)
		return r.cutover(ctx, p, sm)
	}

	return r.startValidationJobs(ctx, p, nodes)
}

// mayWaitForConsumers reports whether enough of the Validating phase is left to keep
// waiting for a consumer pod and still run the Jobs afterwards.
//
// It is how maxConsumerWait is spent: the phase is bounded by that wait plus a full
// Job deadline (see [phaseBound]), so "leave the Jobs their time" and "wait at most
// maxConsumerWait" are the same rule stated once. Waiting up to the phase's own
// deadline instead would mean finding the consumer just as the phase expires, with no
// time left to validate anything on it.
func mayWaitForConsumers(sm *statemachine.Machine[phase]) bool {
	deadline, ok := sm.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > validationJobDeadline
}

// startValidationJobs creates a validation Job on each node and records them in
// status. Existing entries are kept, so it also serves to add nodes that appeared
// after the first round (see cutover's pre-cutover re-check).
func (r *Reconciler) startValidationJobs(
	ctx context.Context,
	p *migrationPass,
	nodes []string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	vm := p.vm

	// Get the simplyblock-rebalancer image from the StorageCluster (it contains nvme-cli).
	image, err := r.resolveRebalancerImage(ctx, vm.Namespace, vm.Status.ClusterUUID)
	if err != nil {
		log.Error(err, "Cannot resolve simplyblock-rebalancer image; requeuing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	have := make(map[string]struct{}, len(vm.Status.ValidationJobs))
	for _, vj := range vm.Status.ValidationJobs {
		have[vj.Node] = struct{}{}
	}

	added := make([]simplyblockv1alpha1.ValidationJob, 0, len(nodes))
	for _, node := range nodes {
		if _, ok := have[node]; ok {
			continue
		}
		job := r.buildValidationJob(vm, node, image)
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create validation job for node %q: %w", node, err)
		}
		added = append(added, simplyblockv1alpha1.ValidationJob{Node: node, JobName: job.Name})
	}
	if len(added) == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	vm.Status.ValidationJobs = append(vm.Status.ValidationJobs, added...)
	for _, vj := range added {
		log.Info("Validation job created", "job", vj.JobName, "node", vj.Node,
			"subsystem", vm.Status.SubsystemNQN, "connections", len(vm.Status.Connections))
	}
	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "ValidationStarted", "ValidationStarted",
		"Validating NVMe paths of subsystem %s on %d node(s): %s",
		vm.Status.SubsystemNQN, len(vm.Status.ValidationJobs), strings.Join(nodes, ", "))
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// fail ends the migration with reason. Nothing is taken back: this is the route for a
// migration that never reached the storage API, or one the storage API has already
// finished with.
func (r *Reconciler) fail(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
	reason string,
) (ctrl.Result, error) {
	p.failure = reason
	if err := sm.TransitionTo(ctx, phaseFailed); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// cancelAndFail ends the migration with reason and takes back what submitting it
// created: the control plane's target-side objects, and the NVMe paths every consumer
// node connected in order to validate. Used where the operator gives up on a
// migration it created and the storage API still believes in.
func (r *Reconciler) cancelAndFail(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
	reason string,
) (ctrl.Result, error) {
	p.cancelBackend = true
	p.releasePaths = true
	return r.fail(ctx, p, sm, reason)
}

// pollValidationJobs waits for every node's validation Job. The migration cuts over
// only once all of them have succeeded; the first failure cancels it. Each Job's pod
// log is collected as it finishes, so the operator log shows per node whether paths
// were connected and validated or the node turned out to have no connection.
func (r *Reconciler) pollValidationJobs(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	vm := p.vm

	pending := 0
	for i := range vm.Status.ValidationJobs {
		vj := &vm.Status.ValidationJobs[i]
		// A node whose validation already passed is not looked at again. Its Job is
		// left in place to be reaped by its TTL, and re-reading a reaped Job would
		// otherwise look like "never validated" and start the whole node over.
		if vj.Succeeded {
			continue
		}

		job := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: vj.JobName}, job); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("get validation job %q: %w", vj.JobName, err)
			}
			// The Job vanished before we observed a terminal state (eviction, manual
			// deletion, ...). Drop the entry and requeue so validateMigration rebuilds
			// it instead of getting wedged in Validating.
			log.Info("Validation job no longer exists; recreating",
				"job", vj.JobName, "node", vj.Node, "migration", vm.Status.MigrationUUID)
			return r.forgetValidationJob(vm, vj.JobName), nil
		}

		// Determine terminal state from Job conditions (set by the Job controller).
		var succeeded, failed bool
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				succeeded = true
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				failed = true
			}
		}
		switch {
		case failed:
			// The Job is left for post-mortem; its pod log is copied into the operator
			// log here because the pod goes away with the Job's TTL.
			r.collectAndLogJobPodLogs(ctx, job)
			log.Error(nil, "Validation job failed; cancelling migration",
				"job", vj.JobName, "node", vj.Node, "migration", vm.Status.MigrationUUID)
			return r.cancelAndFail(ctx, p, sm, fmt.Sprintf(
				"NVMe path validation failed on node %s; migration cancelled", vj.Node))
		case succeeded:
			r.collectAndLogJobPodLogs(ctx, job)
			// Recorded, not merely observed: the pass is persisted with the rest of
			// this reconcile, so an operator restart does not re-run validation on a
			// node that already passed.
			vj.Succeeded = true
		default:
			// Still in progress — we will be re-triggered via Owns(&batchv1.Job{}).
			pending++
		}
	}

	if pending > 0 {
		log.Info("Waiting for NVMe path validation", "pending", pending,
			"nodes", len(vm.Status.ValidationJobs), "migration", vm.Status.MigrationUUID)
		return ctrl.Result{}, nil
	}

	log.Info("All validation jobs succeeded; cutting the migration over",
		"nodes", len(vm.Status.ValidationJobs), "migration", vm.Status.MigrationUUID)

	return r.cutover(ctx, p, sm)
}

// releaseMigrationPaths starts a release Job on every node this migration validated on,
// for a migration that is being given up before cutover.
//
// It is what closes the gap a per-node release cannot. The Job that fails releases its
// own paths on the way out, but the nodes whose validation *passed* exited successfully
// and are never told that the migration was cancelled anyway — by another node's failure,
// or by the operator giving up on a consumer that never started. Their target paths stay
// connected, retry a target that has stopped answering for them, and settle into the husk
// that blocks the next migration of the subsystem. Before this, nothing on the node ever
// took them back.
//
// Every recorded node is asked, not only the ones that passed. Release is idempotent and
// declines to touch a path that is serving, so asking a node that already released costs
// one Job and reports nothing; guessing which nodes still hold paths would mean trusting
// Succeeded to mean "connected", which it does not — a Job killed mid-run leaves paths
// with no record of them at all.
//
// Best effort, and deliberately not waited on: the migration's outcome is already decided
// and must not become "still failing" because a cleanup Job is pending. The Jobs are
// owned by the CR, so they are garbage-collected with it, and carry a TTL of their own.
// What escapes this is caught by the reap that runs before the next validation.
func (r *Reconciler) releaseMigrationPaths(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) {
	log := logf.FromContext(ctx)

	if len(vm.Status.ValidationJobs) == 0 || len(vm.Status.Connections) == 0 {
		// Nothing was validated, so no target path was connected from here.
		return
	}

	image, err := r.resolveRebalancerImage(ctx, vm.Namespace, vm.Status.ClusterUUID)
	if err != nil {
		log.Error(err, "Cannot resolve simplyblock-rebalancer image; migration target paths are left connected",
			"migration", vm.Status.MigrationUUID, "subsystem", vm.Status.SubsystemNQN)
		return
	}

	nodes := make([]string, 0, len(vm.Status.ValidationJobs))
	for _, vj := range vm.Status.ValidationJobs {
		job := r.buildReleaseJob(vm, vj.Node, image)
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			log.Error(err, "Cannot start release job; migration target paths are left connected on this node",
				"node", vj.Node, "migration", vm.Status.MigrationUUID)
			continue
		}
		nodes = append(nodes, vj.Node)
	}
	if len(nodes) == 0 {
		return
	}

	log.Info("Releasing migration target paths", "nodes", nodes,
		"subsystem", vm.Status.SubsystemNQN, "migration", vm.Status.MigrationUUID)
	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "ReleasingPaths", "ReleasingPaths",
		"Releasing the target paths of subsystem %s on %d node(s): %s",
		vm.Status.SubsystemNQN, len(nodes), strings.Join(nodes, ", "))
}

// deleteValidationJobs removes the validation Jobs of a migration. Called when the
// migration leaves the Validating phase — the Jobs have served their purpose and their
// logs are already in the operator log.
func (r *Reconciler) deleteValidationJobs(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) {
	for _, vj := range vm.Status.ValidationJobs {
		_ = r.Delete(ctx,
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: vj.JobName, Namespace: vm.Namespace}},
			client.PropagationPolicy(metav1.DeletePropagationBackground),
		)
	}
}

// validateLateNodes re-resolves the consuming nodes and starts validation for any that
// have no Job yet, returning wait=true while those Jobs run. It reports no error when
// the node set cannot be re-derived: the already-validated set is what we have, and
// blocking a migration that is otherwise ready to cut over on a transient listing
// failure trades a certain delay for an uncertain gain.
//
// A node that disappeared from the set is left alone — its Job either already passed
// (a connected path it no longer needs is harmless) or it never mattered.
func (r *Reconciler) validateLateNodes(
	ctx context.Context,
	p *migrationPass,
) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)
	vm := p.vm

	// Nothing was validated (idle subsystem) — nothing to re-check either.
	if len(vm.Status.ValidationJobs) == 0 {
		return ctrl.Result{}, false, nil
	}

	nodes, err := r.resolveValidationNodes(ctx, vm)
	if err != nil {
		log.Info("Cannot re-check the nodes to validate before cutover; continuing with the validated set",
			"migration", vm.Status.MigrationUUID, "reason", err.Error())
		return ctrl.Result{}, false, nil
	}

	validated := make(map[string]struct{}, len(vm.Status.ValidationJobs))
	for _, vj := range vm.Status.ValidationJobs {
		validated[vj.Node] = struct{}{}
	}
	var late []string
	for _, node := range nodes {
		if _, ok := validated[node]; !ok {
			late = append(late, node)
		}
	}
	if len(late) == 0 {
		return ctrl.Result{}, false, nil
	}

	log.Info("New consuming node(s) appeared during validation; validating them before cutover",
		"nodes", late, "subsystem", vm.Status.SubsystemNQN, "migration", vm.Status.MigrationUUID)
	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "ValidationExtended", "ValidationExtended",
		"Subsystem %s gained consumer(s) on %s during validation; validating before cutover",
		vm.Status.SubsystemNQN, strings.Join(late, ", "))
	res, err := r.startValidationJobs(ctx, p, nodes)
	return res, err == nil, err
}

// forgetValidationJob removes one Job from status and requeues, so the next
// reconcile recreates it for that node.
func (r *Reconciler) forgetValidationJob(
	vm *simplyblockv1alpha1.VolumeMigration,
	jobName string,
) ctrl.Result {
	kept := make([]simplyblockv1alpha1.ValidationJob, 0, len(vm.Status.ValidationJobs))
	for _, vj := range vm.Status.ValidationJobs {
		if vj.JobName != jobName {
			kept = append(kept, vj)
		}
	}
	vm.Status.ValidationJobs = kept
	return ctrl.Result{Requeue: true}
}

// cutover moves a validated (or validation-skipped) migration to Running, which is
// where the data actually starts moving. The work of getting there is the Running
// phase's entry hook; this decides whether it is time.
func (r *Reconciler) cutover(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Re-derive the nodes that need paths, right before the point of no return. The
	// control plane lets a new volume join the subsystem until the migration is
	// activated, and a consumer pod can be rescheduled while we validate — either way
	// a node can appear that was not validated. Validate it too before cutting over.
	if res, wait, err := r.validateLateNodes(ctx, p); err != nil {
		return res, err
	} else if wait {
		return res, nil
	}

	err := sm.TransitionTo(ctx, phaseRunning)
	switch {
	case err == nil:
		return ctrl.Result{RequeueAfter: MigrationInitialDelay}, nil
	case errors.Is(err, errMigrationStartFailed):
		// The migration provably never left pre_created and the hook has already
		// cancelled it on the control plane; the host-side paths are still ours to give
		// back. There is nothing to retry — this migration is not going to start.
		p.releasePaths = true
		return r.fail(ctx, p, sm, err.Error())
	default:
		// The storage API did not answer, so whether the migration was continued is
		// unknown. Stay in Validating — paths connected, phase bound still running —
		// and ask again; the hook re-reads the backend phase and will not continue
		// twice.
		log.Error(err, "Cannot cut the migration over yet; requeuing",
			"migration", p.vm.Status.MigrationUUID)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
}

// buildValidationJob constructs the Job that connects NVMe paths and validates
// ANA state on the target node using the simplyblock-rebalancer binary.
func (r *Reconciler) buildValidationJob(
	vm *simplyblockv1alpha1.VolumeMigration,
	hostname, image string,
) *batchv1.Job {
	// No retries: a failed validation cancels the migration, and retrying the Job would
	// only delay that decision behind a second run of a check that just said no.
	return migrationPathJob(vm, hostname, image, pathJobSpec{
		namePrefix:   "vmig-validate-",
		container:    "nvme-validate",
		mode:         "validate-migration",
		backoffLimit: 0,
	})
}

// buildReleaseJob constructs the Job that gives a migration's target paths back on one
// node, for the nodes the operator has to tell because their own Job cannot know.
func (r *Reconciler) buildReleaseJob(
	vm *simplyblockv1alpha1.VolumeMigration,
	hostname, image string,
) *batchv1.Job {
	// Retried, unlike validation: nothing downstream waits on this Job, so a transient
	// failure that is not retried is simply a path left connected, which is the leak.
	return migrationPathJob(vm, hostname, image, pathJobSpec{
		namePrefix:   "vmig-release-",
		container:    "nvme-release",
		mode:         "release-migration-paths",
		backoffLimit: 2,
	})
}

// pathJobSpec is what differs between the two host-side migration path Jobs. Everything
// else about them — the node pinning, the host mounts, the privileges, the VMIG_
// environment — has to be identical, because both are the same binary reading the same
// host state, and a release that saw a different fabric than the validation did would be
// deciding about paths it cannot see.
type pathJobSpec struct {
	namePrefix   string
	container    string
	mode         string
	backoffLimit int32
}

// migrationPathJob builds one node-pinned Job running simplyblock-rebalancer against the
// host's NVMe fabric.
func migrationPathJob(
	vm *simplyblockv1alpha1.VolumeMigration,
	hostname, image string,
	js pathJobSpec,
) *batchv1.Job {
	privileged := true
	readOnly := true
	ttl := int32(3600)
	deadline := int64(validationJobDeadline.Seconds())

	connsJSON, _ := json.Marshal(connectionsToValidation(vm.Status.Connections))

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			// One Job per node, so the name carries both the migration and the node.
			Name:      js.namePrefix + utils.SafeNodeID(vm.Status.MigrationUUID) + "-" + utils.NodeNameSuffix(hostname),
			Namespace: vm.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vm, simplyblockv1alpha1.GroupVersion.WithKind("VolumeMigration")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &js.backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": hostname},
					HostNetwork:   true,
					Volumes: []corev1.Volume{
						{
							Name: "host-dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/dev"},
							},
						},
						{
							// The subsystem presence check reads the host's NVMe sysfs
							// (/sys/class/nvme-subsystem); the container's own /sys is
							// not the host's.
							Name: "host-sys",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/sys"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            js.container,
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"simplyblock-rebalancer", "--mode=" + js.mode},
							Env: []corev1.EnvVar{
								{Name: "VMIG_CONNECTIONS", Value: string(connsJSON)},
								// Which subsystem this node is expected to have a
								// connection to; empty means "skip the check".
								{Name: "VMIG_SUBSYSTEM_NQN", Value: vm.Status.SubsystemNQN},
								// The host's sysfs, mounted below — the container's own
								// /sys is not guaranteed to be it.
								{Name: "VMIG_SYS_ROOT", Value: "/host/sys"},
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-dev", MountPath: "/dev"},
								{Name: "host-sys", MountPath: "/host/sys", ReadOnly: readOnly},
							},
						},
					},
				},
			},
		},
	}
}

// connectionsToValidation converts MigrationConnection status entries to the
// Connection type consumed by the simplyblock-rebalancer validate-migration mode.
func connectionsToValidation(conns []simplyblockv1alpha1.MigrationConnection) []Connection {
	out := make([]Connection, len(conns))
	for i, c := range conns {
		out[i] = Connection{
			NQN:            c.NQN,
			IP:             c.IP,
			Port:           c.Port,
			Transport:      c.Transport,
			NrIoQueues:     c.NrIoQueues,
			ReconnectDelay: c.ReconnectDelay,
			CtrlLossTmo:    c.CtrlLossTmo,
			FastIOFailTmo:  c.FastIOFailTmo,
			KeepAliveTmo:   c.KeepAliveTmo,
		}
	}
	return out
}

// resolveConsumerNodeName finds the Kubernetes node name of the worker node
// running a pod that currently has the PVC (resolved from pvName) mounted.
// NVMe connections must be established from that node so that the consuming
// pod can reach the target subsystem after migration.
//
// It distinguishes three consumer states so the caller can act safely:
//   - (&node, nil): a Running consumer pod was found; its node name is returned.
//   - (nil, errConsumerNotReady): a pod references the PVC but is not Running yet
//     — a consumer is coming, so the caller must wait, not skip validation.
//   - (nil, nil): the PVC is unbound or no pod references it at all — genuinely
//     idle, so there is nothing to validate and the caller can skip validation.
//
// Any other non-nil error is a genuine, retryable failure (PV get or pod list
// failed). Reads go through the uncached apiReader: a stale cache could otherwise
// miss a running consumer and cause validation to be skipped for a live volume.
// resolveValidationNodes returns every worker node that consumes a volume of the
// migrated subsystem — the nodes whose NVMe paths must be switched to the target
// before cutover. A subsystem moves as a unit, so validating only the node of the
// volume named in the spec leaves every sibling's consumer pointing at the source:
// at cutover those hosts lose their volume.
//
// Membership comes from the control plane (volumes sharing the subsystem's NQN) and
// is mapped back to PVs through the CSI volume handle, then to consumers through the
// PVC. Members whose PV or consumer cannot be found contribute no node; a member with
// a not-yet-Running consumer propagates errConsumerNotReady so the caller waits,
// because a pod that stages against the source mid-migration is stranded at cutover
// exactly like an established one.
//
// The returned node list is sorted, so a Job set built from it is stable across
// reconciles.
func (r *Reconciler) resolveValidationNodes(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) ([]string, error) {
	log := logf.FromContext(ctx)

	members, err := r.apiClient.GetSubsystemVolumes(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN)
	if err != nil {
		return nil, err
	}
	memberUUIDs := make(map[string]struct{}, len(members))
	for _, m := range members {
		memberUUIDs[m.UUID] = struct{}{}
	}
	// The migrated volume is always a member, even if the listing raced a change.
	memberUUIDs[vm.Status.VolumeUUID] = struct{}{}

	pvNames, err := r.pvNamesForVolumes(ctx, memberUUIDs)
	if err != nil {
		return nil, err
	}
	if len(pvNames) < len(memberUUIDs) {
		// A member without a PV is not consumed through this cluster's CSI driver, so
		// it has no host paths to validate — but it is worth saying out loud, since it
		// also means we cannot vouch for whatever else uses it.
		log.Info("Some subsystem members have no PersistentVolume in this cluster",
			"subsystem", vm.Status.SubsystemNQN, "members", len(memberUUIDs), "pvs", len(pvNames))
	}

	nodes := make(map[string]string, len(pvNames)) // node -> the PV that put it in the set
	for _, pvName := range pvNames {
		node, err := r.resolveConsumerNodeName(ctx, pvName)
		if err != nil {
			// errConsumerNotReady included: wait for every member's consumer.
			return nil, fmt.Errorf("resolve consumer of %s: %w", pvName, err)
		}
		if node == nil {
			continue // genuinely idle member: no host paths to validate
		}
		if _, seen := nodes[*node]; !seen {
			nodes[*node] = pvName
		}
	}

	out := make([]string, 0, len(nodes))
	for node := range nodes {
		out = append(out, node)
	}
	sort.Strings(out)
	return out, nil
}

// pvNamesForVolumes maps backend volume UUIDs to the PersistentVolumes that front
// them, by parsing each PV's CSI volume handle ("<cluster>:<pool>:<volume>").
func (r *Reconciler) pvNamesForVolumes(
	ctx context.Context,
	volumeUUIDs map[string]struct{},
) ([]string, error) {
	var pvs corev1.PersistentVolumeList
	if err := r.apiReader.List(ctx, &pvs); err != nil {
		return nil, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	var names []string
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI == nil {
			continue
		}
		parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if _, ok := volumeUUIDs[parts[2]]; ok {
			names = append(names, pv.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (r *Reconciler) resolveConsumerNodeName(
	ctx context.Context,
	pvName string,
) (*string, error) {
	pv := &corev1.PersistentVolume{}
	if err := r.apiReader.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		return nil, fmt.Errorf("get PV %q: %w", pvName, err)
	}
	if pv.Spec.ClaimRef == nil {
		return nil, nil
	}
	pvcName := pv.Spec.ClaimRef.Name
	pvcNamespace := pv.Spec.ClaimRef.Namespace

	// to ensure we don't miss a running consumer, we read from the uncached apiReader
	var podList corev1.PodList
	if err := r.apiReader.List(ctx, &podList, client.InNamespace(pvcNamespace)); err != nil {
		return nil, fmt.Errorf("list pods in namespace %q: %w", pvcNamespace, err)
	}

	// referenced tracks whether any pod (regardless of phase) consumes the PVC,
	// so we can tell "workload stopped" (skip) apart from "consumer not ready yet"
	// (wait).
	referenced := false
	for _, pod := range podList.Items {
		usesPVC := false
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
				usesPVC = true
				break
			}
		}
		if !usesPVC {
			continue
		}
		referenced = true
		if pod.Spec.NodeName != "" && pod.Status.Phase == corev1.PodRunning {
			return &pod.Spec.NodeName, nil
		}
	}
	if referenced {
		return nil, fmt.Errorf("consumer pod for PVC %q/%q is not running yet: %w", pvcNamespace, pvcName, errConsumerNotReady)
	}
	// No pod references the PVC at all (workload stopped / never scheduled): the
	// volume is genuinely idle, so signal "nothing to validate" with (nil, nil).
	return nil, nil
}

// collectAndLogJobPodLogs fetches stdout/stderr from every pod that belongs to
// the given Job and emits them as operator log lines. Must be called before
// deleting the Job, since pod deletion follows immediately after.
func (r *Reconciler) collectAndLogJobPodLogs(ctx context.Context, job *batchv1.Job) {
	log := logf.FromContext(ctx).WithValues("job", job.Name)

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		log.Error(err, "Failed to list validation job pods for log collection")
		return
	}
	for _, pod := range podList.Items {
		req := r.coreClient.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
		stream, err := req.Stream(ctx)
		if err != nil {
			log.Error(err, "Failed to stream logs from validation pod", "pod", pod.Name)
			continue
		}
		buf := new(bytes.Buffer)
		_, _ = io.Copy(buf, stream)
		_ = stream.Close()
		log.Info("Validation job pod output", "pod", pod.Name, "logs", buf.String())
	}
}

// resolveRebalancerImage returns the simplyblock-rebalancer image for the StorageCluster
// that owns the migration's volume. Volume migration is enabled by default: an omitted
// VolumeMigrationSettings block (or one without an image) resolves to
// [defaultRebalancerImage]; only an explicit Enabled=false disables it.
//
// That default is the same one [GetConfig] hands the rebalancer deployment, so both
// honour RebalancerImageEnvVar — which the Helm chart pins to the operator's own tag.
// These Jobs run the rebalancer binary, so it is the tag they want: an operator that
// validates NVMe paths with a differently-versioned binary is testing something other
// than what it ships.
func (r *Reconciler) resolveRebalancerImage(
	ctx context.Context,
	namespace, clusterUUID string,
) (string, error) {
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list StorageClusters: %w", err)
	}
	for _, cr := range clusters.Items {
		if cr.Status.UUID != clusterUUID {
			continue
		}
		vm := cr.Spec.VolumeMigrationSettings
		if vm == nil {
			// No settings block: volume migration is enabled by default with the default image.
			return defaultRebalancerImage(), nil
		}
		if vm.Enabled != nil && !*vm.Enabled {
			return "", fmt.Errorf("volume migration is disabled for cluster UUID %q", clusterUUID)
		}
		if vm.RebalancerImage != nil && *vm.RebalancerImage != "" {
			return *vm.RebalancerImage, nil
		}
		// Enabled (explicitly or by default) but no image pinned: use the default image.
		return defaultRebalancerImage(), nil
	}
	return "", fmt.Errorf("no StorageCluster found for cluster UUID %q", clusterUUID)
}

// pollMigration follows a migration the storage API is working on, updating progress
// in status and moving the machine when the API reports it finished. Most passes
// change nothing but the counters: the phase is not bounded by a deadline, because a
// data copy takes as long as there is data, so what ends it is the storage API.
func (r *Reconciler) pollMigration(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	vm := p.vm

	// StartedAt is an optional pointer and may be nil on older objects, manual
	// edits, or partial status writes. Backfill it rather than dereferencing a
	// nil pointer (panic) or defaulting to the zero time (instant "stuck" warning).
	if vm.Status.StartedAt == nil {
		now := metav1.Now()
		vm.Status.StartedAt = &now
		log.Info("StartedAt was unset in Running phase; backfilled", "migration", vm.Status.MigrationUUID)
	}

	result, err := PollMigration(ctx, r.apiClient, vm.Status.ClusterUUID,
		vm.Status.SubsystemNQN, vm.Status.MigrationUUID, vm.Status.StartedAt.Time)
	if err != nil {
		log.Error(err, "Cannot poll migration; requeuing", "migration", vm.Status.MigrationUUID)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if result.Migration != nil {
		// Progress, whether or not the migration is done yet.
		vm.Status.SourceNodeUUID = result.Migration.SourceNodeID
		vm.Status.MemberCount = result.Migration.MemberCount
	}

	if result.Stuck {
		r.Recorder.Eventf(vm, nil, corev1.EventTypeWarning, "MigrationStuck", "MigrationStuck",
			"Migration %s has not completed after %s (phase: %s, status: %s)",
			vm.Status.MigrationUUID, MigrationStuckWarningTimeout,
			result.Migration.Phase, result.Migration.Status)
	}

	if !result.Done {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Finished, one way or the other. The outcome goes to the hook, so a failure is
	// recorded with the storage API's own error message rather than one made up here.
	p.finished = result.Migration
	to := phaseCompleted
	if !result.Succeeded {
		to = phaseFailed
	}
	if err := sm.TransitionTo(ctx, to); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// MarkVolumeMoved increments status.volumeMoveGeneration on the StorageCluster whose
// backend UUID matches clusterUUID, recording that one more volume has moved and a
// control-plane data realignment is owed. The counter is read by the operator's
// rebalancing loop, which compares it against status.realignedGeneration.
//
// Exported, and a function rather than a method, because it is the one thing a
// completing migration tells the rest of the operator, and the loop on the other end
// of the counter has to be able to test against the real writer rather than a
// hand-rolled increment that could drift from it.
//
// A counter rather than a flag because both quantities matter: how many moves are
// outstanding (so DataRealignment.MinMoves can batch them) and whether a move landed
// after a realignment was already requested (so it is not silently absorbed by a
// realignment that cannot account for it).
//
// Best-effort: any failure is logged but does not fail the migration — the realignment
// is late, not lost, because the next completed migration increments again. The write
// takes the optimistic-locking path and retries on conflict, since two migrations
// completing at once would otherwise read the same value and one increment would
// vanish; with MinMoves batching, a lost increment delays a realignment indefinitely
// rather than by one cycle.
func MarkVolumeMoved(
	ctx context.Context,
	c client.Client,
	namespace, clusterUUID string,
) {
	log := logf.FromContext(ctx)
	if clusterUUID == "" {
		return
	}

	var name string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var clusters simplyblockv1alpha1.StorageClusterList
		if err := c.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
			return err
		}
		for i := range clusters.Items {
			cr := &clusters.Items[i]
			if cr.Status.UUID != clusterUUID {
				continue
			}
			name = cr.Name
			patch := client.MergeFromWithOptions(cr.DeepCopy(), client.MergeFromWithOptimisticLock{})
			cr.Status.VolumeMoveGeneration = ptr.To(ptr.Int64FromOrZero(cr.Status.VolumeMoveGeneration) + 1)
			return c.Status().Patch(ctx, cr, patch)
		}
		return nil
	})
	switch {
	case err != nil:
		log.Error(err, "Cannot record volume move for realignment", "clusterUUID", clusterUUID, "cluster", name)
	case name == "":
		log.Info("No StorageCluster matched volume's cluster UUID; cannot record volume move",
			"clusterUUID", clusterUUID)
	}
}

// abort stops a migration the user asked to stop, by moving it to Aborted.
//
// Because the graph declares which phases can be aborted, an abort arriving for a
// phase that never submitted anything is refused by the machine rather than by a
// hand-written check — and that is a different outcome from a backend failure, worth
// telling apart: there is nothing to cancel, so the migration is failed outright
// instead of being reported as an abort that never reached anything.
func (r *Reconciler) abort(
	ctx context.Context,
	p *migrationPass,
	sm *statemachine.Machine[phase],
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Aborting migration", "migration", p.vm.Status.MigrationUUID,
		"phase", sm.CurrentState())

	err := sm.TransitionTo(ctx, phaseAborted)
	illegal, refused := errors.AsType[*statemachine.IllegalTransitionError[phase]](err)
	switch {
	case err == nil:
		return ctrl.Result{}, nil
	case refused:
		return r.fail(ctx, p, sm, fmt.Sprintf("aborted while still %q", illegal.From))
	default:
		// CancelMigration itself failed. The migration keeps running until the control
		// plane says otherwise, so ask again rather than claim it stopped.
		log.Error(err, "Cannot cancel migration; retrying")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *Reconciler) SetupWithManager(
	mgr ctrl.Manager,
) error {
	r.apiClient = webapi.NewClient()
	k8s, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("create k8s client for log collection: %w", err)
	}
	r.coreClient = k8s.CoreV1()
	// Uncached reader for the consumer-detection decision (see resolveConsumerNodeName).
	r.apiReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.VolumeMigration{}).
		Owns(&batchv1.Job{}).
		Named("volumemigration").
		Complete(r)
}
