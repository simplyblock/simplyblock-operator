package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/simplyblock/atlas/ptr"
	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
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

// VolumeMigrationReconciler reconciles VolumeMigration resources.
type VolumeMigrationReconciler struct {
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

func (r *VolumeMigrationReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	vm := &simplyblockv1alpha1.VolumeMigration{}
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal phases — nothing left to do.
	switch vm.Status.Phase {
	case simplyblockv1alpha1.VolumeMigrationPhaseCompleted,
		simplyblockv1alpha1.VolumeMigrationPhaseFailed,
		simplyblockv1alpha1.VolumeMigrationPhaseAborted:
		return ctrl.Result{}, nil
	}

	// Abort applies to any migration that has not finished, including one still
	// pending — a migration deferred by a busy cluster would otherwise keep retrying
	// for the whole deferral window while the user has already asked it to stop.
	if vm.Spec.Abort {
		return r.reconcileAbort(ctx, vm)
	}

	switch vm.Status.Phase {
	case simplyblockv1alpha1.VolumeMigrationPhaseRunning:
		return r.reconcileRunning(ctx, vm)
	case simplyblockv1alpha1.VolumeMigrationPhaseValidating:
		return r.reconcileValidating(ctx, vm)
	default:
		return r.reconcileStart(ctx, vm)
	}
}

// reconcileStart resolves the PV to a logical volume, finds its cluster/pool,
// and submits the migration to the storage API.
func (r *VolumeMigrationReconciler) reconcileStart(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Resolve PV → volume UUID via CSI volume handle.
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: vm.Spec.PVName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setFailed(ctx, vm, fmt.Sprintf("PersistentVolume %q not found", vm.Spec.PVName))
		}
		return ctrl.Result{}, fmt.Errorf("get PV %q: %w", vm.Spec.PVName, err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return r.setFailed(ctx, vm, fmt.Sprintf("PV %q has no CSI volume handle", vm.Spec.PVName))
	}
	// CSI volume handle format: "<clusterUUID>:<poolUUID>:<volumeUUID>"
	parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return r.setFailed(ctx, vm, fmt.Sprintf("PV %q has unexpected CSI volume handle format %q (expected <clusterUUID>:<poolUUID>:<volumeUUID>)", vm.Spec.PVName, pv.Spec.CSI.VolumeHandle))
	}
	clusterUUID, poolUUID, volumeUUID := parts[0], parts[1], parts[2]

	if _, err := r.resolveRebalancerImage(ctx, vm.Namespace, clusterUUID); err != nil {
		return r.setFailed(ctx, vm, fmt.Sprintf("volume migration not enabled/configured for cluster %q: %v", clusterUUID, err))
	}

	// The storage API migrates a whole NVMe subsystem, addressed by its NQN, so
	// resolve the volume to its subsystem before submitting. For a namespaced
	// volume the subsystem is shared and its sibling volumes move along.
	volume, err := r.apiClient.GetVolume(ctx, clusterUUID, poolUUID, volumeUUID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get volume %q: %w", volumeUUID, err)
	}
	if volume == nil {
		return r.setFailed(ctx, vm, fmt.Sprintf("volume %s no longer exists", volumeUUID))
	}
	if volume.NQN == "" {
		return r.setFailed(ctx, vm, fmt.Sprintf("volume %s has no subsystem NQN; cannot address its migration", volumeUUID))
	}

	log.Info("Submitting volume migration",
		"volume", volumeUUID, "cluster", clusterUUID, "subsystem", volume.NQN,
		"target", vm.Spec.TargetNodeUUID)

	migration, err := r.apiClient.CreateMigration(ctx, clusterUUID, volume.NQN, vm.Spec.TargetNodeUUID)
	switch {
	case errors.Is(err, webapi.ErrMigrationNotAcceptingYet):
		return r.deferMigration(ctx, vm, clusterUUID, err)
	case isIndeterminateCreate(err):
		// The request never got an answer, so whether the control plane created the
		// migration is unknown — and it may well have: a create can take longer than the
		// client timeout while a rebalance is in flight, and it allocates bdevs on the way.
		// Failing here would abandon that half-created migration. Retrying instead lets the
		// next attempt hit the existing-migration path, which cancels it before re-creating.
		return r.retryIndeterminateCreate(ctx, vm, clusterUUID, err)
	case err != nil:
		return r.setFailed(ctx, vm, fmt.Sprintf("CreateMigration: %v", err))
	}
	if migration == nil {
		// Previous migration had to be canceled, retry in the next reconcile cycle.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if migration.ID == "" {
		return r.setFailed(ctx, vm, "CreateMigration returned empty migration UUID")
	}

	now := metav1.Now()
	conns := make([]simplyblockv1alpha1.MigrationConnection, 0, len(migration.ConnectStrings))
	for _, c := range migration.ConnectStrings {
		conns = append(conns, simplyblockv1alpha1.MigrationConnection{
			NQN:            c.Nqn,
			IP:             c.IP,
			Port:           c.Port,
			Transport:      c.TargetType,
			NrIoQueues:     c.NrIoQueues,
			ReconnectDelay: c.ReconnectDelay,
			// Not c.CtrlLossTmo: the host connects every path with the same loss
			// timeout the CSI driver uses, and the control plane's answer here is an
			// hour. See vmigration.CtrlLossTmoSec. Overridden at ingestion rather than
			// where the Job is built so that status.connections records the connect
			// that will actually be made.
			CtrlLossTmo:   vmigration.CtrlLossTmoSec,
			FastIOFailTmo: c.FastIOFailTmo,
			KeepAliveTmo:  c.KeepAliveTmo,
		})
	}

	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseValidating
	vm.Status.DeferredSince = nil // accepted; the deferral window no longer applies
	vm.Status.MigrationUUID = migration.ID
	vm.Status.ClusterUUID = clusterUUID
	vm.Status.VolumeUUID = volumeUUID
	vm.Status.PoolUUID = poolUUID
	// Persisted so every later call — continue, poll, cancel — can address the
	// migration without resolving the volume again.
	vm.Status.SubsystemNQN = volume.NQN
	vm.Status.MemberCount = migration.MemberCount
	// SourceNodeUUID is populated from GetMigration once status=Running.
	vm.Status.Connections = conns
	vm.Status.StartedAt = &now
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Validating: %w", err)
	}

	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationCreated", "MigrationCreated",
		"Migration %s created for subsystem %s (%d volume(s)): validating %d connection(s) to node %s",
		migration.ID, volume.NQN, migration.MemberCount, len(conns), vm.Spec.TargetNodeUUID)
	return ctrl.Result{Requeue: true}, nil
}

// deferMigration holds a migration the control plane refused because the cluster is
// busy with work that ends on its own, and retries it — typically the data realignment
// a previous migration triggered, which the control plane will not migrate through.
//
// The wait is bounded by maxMigrationDeferral, measured from the first refusal (which
// is why it is recorded in status rather than kept in memory: the operator may restart,
// and an observer needs to see that the migration is waiting and since when). Past that
// window the migration fails with the control plane's own reason, so whatever is waiting
// on this CR learns about it instead of hanging on a phase that never becomes terminal.
func (r *VolumeMigrationReconciler) deferMigration(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
	clusterUUID string,
	cause error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if vm.Status.DeferredSince == nil {
		now := metav1.Now()
		patch := client.MergeFrom(vm.DeepCopy())
		vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhasePending
		vm.Status.DeferredSince = &now
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch status Pending (deferred): %w", err)
		}
	} else if waited := time.Since(vm.Status.DeferredSince.Time); waited > maxMigrationDeferral {
		return r.setFailed(ctx, vm, fmt.Sprintf(
			"cluster %s did not accept the migration within %s: %v", clusterUUID, maxMigrationDeferral, cause))
	}

	waited := time.Since(vm.Status.DeferredSince.Time).Round(time.Second)
	log.Info("Cluster is not accepting migrations yet; retrying",
		"cluster", clusterUUID, "waited", waited, "giveUpAfter", maxMigrationDeferral,
		"reason", cause.Error())
	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationDeferred", "MigrationDeferred",
		"Cluster %s is not accepting migrations yet (waiting %s of at most %s); retrying in %s",
		clusterUUID, waited, maxMigrationDeferral, migrationDeferredRetryDelay)
	return ctrl.Result{RequeueAfter: migrationDeferredRetryDelay}, nil
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
// it, bounded by the same deferral ceiling: the request may have taken effect, so the CR
// must not be failed until a later attempt can observe (and cancel) what was left behind.
func (r *VolumeMigrationReconciler) retryIndeterminateCreate(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
	clusterUUID string,
	cause error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if vm.Status.DeferredSince == nil {
		now := metav1.Now()
		patch := client.MergeFrom(vm.DeepCopy())
		vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhasePending
		vm.Status.DeferredSince = &now
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch status Pending (create timed out): %w", err)
		}
	} else if waited := time.Since(vm.Status.DeferredSince.Time); waited > maxMigrationDeferral {
		return r.setFailed(ctx, vm, fmt.Sprintf(
			"CreateMigration did not return within %s of retrying on cluster %s: %v",
			maxMigrationDeferral, clusterUUID, cause))
	}

	waited := time.Since(vm.Status.DeferredSince.Time).Round(time.Second)
	log.Info("CreateMigration gave no answer; the migration may exist on the backend, retrying",
		"cluster", clusterUUID, "waited", waited, "giveUpAfter", maxMigrationDeferral,
		"reason", cause.Error())
	r.Recorder.Eventf(vm, nil, corev1.EventTypeWarning, "MigrationCreateIndeterminate",
		"MigrationCreateIndeterminate",
		"CreateMigration on cluster %s returned no answer (%v); it may have taken effect, "+
			"so retrying in %s rather than failing", clusterUUID, cause, migrationDeferredRetryDelay)
	return ctrl.Result{RequeueAfter: migrationDeferredRetryDelay}, nil
}

// reconcileValidating creates one Job per worker node that consumes a volume of the
// migrated subsystem. Each Job:
//  1. Checks whether this node has a host connection to the subsystem at all.
//  2. If it does, runs `nvme connect` for each connection returned by CreateMigration.
//  3. Runs `nvme list --verbose` and verifies all new NQNs appear with ANA
//     state "inaccessible" (paths connected but volume not yet migrated).
//
// Once every Job has succeeded the controller calls ContinueMigration and advances to
// Running. Any Job failing cancels the migration: cutover is subsystem-wide, so
// continuing with a subset of the consumers ready guarantees an outage for the rest.
func (r *VolumeMigrationReconciler) reconcileValidating(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if vm.Status.MigrationUUID == "" {
		return r.setFailed(ctx, vm, "migration UUID is empty in Validating phase; status was likely written before a failed CreateMigration")
	}

	// Jobs already created — poll them.
	if len(vm.Status.ValidationJobs) > 0 {
		return r.pollValidationJobs(ctx, vm)
	}

	nodes, err := r.resolveValidationNodes(ctx, vm)
	switch {
	case errors.Is(err, errConsumerNotReady):
		// A consumer pod exists but is not Running yet. Do not skip validation: wait
		// so its node gets the new paths too. Bounded, because the control plane will
		// not hold a created-but-unstarted migration open indefinitely.
		if waited := r.timeInValidating(vm); waited > maxConsumerWait {
			return r.cancelAndFail(ctx, vm, fmt.Sprintf(
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
		// validate anywhere. Skip validation and continue the migration directly.
		log.Info("No consumer for any volume of the subsystem; skipping NVMe path validation",
			"subsystem", vm.Status.SubsystemNQN, "migration", vm.Status.MigrationUUID)
		r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "ValidationSkipped", "ValidationSkipped",
			"No consumer for any volume of subsystem %s; skipping NVMe path validation",
			vm.Status.SubsystemNQN)
		return r.performMigration(ctx, vm)
	}

	return r.startValidationJobs(ctx, vm, nodes)
}

// startValidationJobs creates a validation Job on each node and records them in
// status. Existing entries are kept, so it also serves to add nodes that appeared
// after the first round (see performMigration's pre-cutover re-check).
func (r *VolumeMigrationReconciler) startValidationJobs(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
	nodes []string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

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

	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.ValidationJobs = append(vm.Status.ValidationJobs, added...)
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch ValidationJobs: %w", err)
	}

	for _, vj := range added {
		log.Info("Validation job created", "job", vj.JobName, "node", vj.Node,
			"subsystem", vm.Status.SubsystemNQN, "connections", len(vm.Status.Connections))
	}
	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "ValidationStarted", "ValidationStarted",
		"Validating NVMe paths of subsystem %s on %d node(s): %s",
		vm.Status.SubsystemNQN, len(vm.Status.ValidationJobs), strings.Join(nodes, ", "))
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// timeInValidating is how long the migration has been in the Validating phase, i.e.
// since it was submitted to the storage API.
func (r *VolumeMigrationReconciler) timeInValidating(
	vm *simplyblockv1alpha1.VolumeMigration,
) time.Duration {
	if vm.Status.StartedAt == nil {
		return 0
	}
	return time.Since(vm.Status.StartedAt.Time)
}

// cancelAndFail cancels the backend migration and fails the CR with reason. Used
// where the operator gives up on a migration it created: the target-side objects must
// not be left behind.
//
// "Target-side objects" is both halves of it. The control plane's are what CancelMigration
// takes back; the host's are the NVMe paths every consumer node connected to validate,
// which nothing else will ever release — the Job that failed releases its own, and the
// ones that passed have to be told.
func (r *VolumeMigrationReconciler) cancelAndFail(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
	reason string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if err := r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID); err != nil {
		// Report the original reason regardless; a failed cancel only adds to it.
		log.Error(err, "Cannot cancel migration; target-side objects may remain",
			"migration", vm.Status.MigrationUUID, "subsystem", vm.Status.SubsystemNQN)
		reason += fmt.Sprintf(" (cancelling the migration also failed: %v)", err)
	}
	// Before setFailed, which clears nothing but is the point of no return for the CR:
	// the node list this needs lives in status.
	r.releaseMigrationPaths(ctx, vm)
	return r.setFailed(ctx, vm, reason)
}

// pollValidationJobs waits for every node's validation Job. The migration continues
// only once all of them have succeeded; the first failure cancels it. Each Job's pod
// log is collected as it finishes, so the operator log shows per node whether paths
// were connected and validated or the node turned out to have no connection.
func (r *VolumeMigrationReconciler) pollValidationJobs(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	patch := client.MergeFrom(vm.DeepCopy())
	pending, newlyPassed := 0, false

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
			// deletion, ...). Drop the entry and requeue so reconcileValidating rebuilds
			// it instead of getting wedged in Validating.
			log.Info("Validation job no longer exists; recreating",
				"job", vj.JobName, "node", vj.Node, "migration", vm.Status.MigrationUUID)
			return r.forgetValidationJob(ctx, vm, vj.JobName)
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
			return r.cancelAndFail(ctx, vm, fmt.Sprintf(
				"NVMe path validation failed on node %s; migration cancelled", vj.Node))
		case succeeded:
			r.collectAndLogJobPodLogs(ctx, job)
			vj.Succeeded = true
			newlyPassed = true
		default:
			// Still in progress — we will be re-triggered via Owns(&batchv1.Job{}).
			pending++
		}
	}

	// Persist the passes before acting on them: an operator restart must not re-run
	// validation on nodes that already passed, and the count below must be the
	// recorded one, not one this pass happens to have observed.
	if newlyPassed {
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("record validation passes: %w", err)
		}
	}

	if pending > 0 {
		log.Info("Waiting for NVMe path validation", "pending", pending,
			"nodes", len(vm.Status.ValidationJobs), "migration", vm.Status.MigrationUUID)
		return ctrl.Result{}, nil
	}

	log.Info("All validation jobs succeeded; calling ContinueMigration",
		"nodes", len(vm.Status.ValidationJobs), "migration", vm.Status.MigrationUUID)

	return r.performMigration(ctx, vm)
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
func (r *VolumeMigrationReconciler) releaseMigrationPaths(
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
func (r *VolumeMigrationReconciler) deleteValidationJobs(
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
func (r *VolumeMigrationReconciler) validateLateNodes(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)

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
	res, err := r.startValidationJobs(ctx, vm, nodes)
	return res, err == nil, err
}

// forgetValidationJob removes one Job from status and requeues, so the next
// reconcile recreates it for that node.
func (r *VolumeMigrationReconciler) forgetValidationJob(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
	jobName string,
) (ctrl.Result, error) {
	kept := make([]simplyblockv1alpha1.ValidationJob, 0, len(vm.Status.ValidationJobs))
	for _, vj := range vm.Status.ValidationJobs {
		if vj.JobName != jobName {
			kept = append(kept, vj)
		}
	}
	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.ValidationJobs = kept
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("drop validation job %q from status: %w", jobName, err)
	}
	return ctrl.Result{Requeue: true}, nil
}

// performMigration advances a validated (or validation-skipped) migration to
// the Running phase. Used both after a successful validation job and when
// validation is skipped (no running consumer).
//
// The backend's ContinueMigration is not idempotent: it only accepts a migration
// in phase "pre_created" and rejects any later call. If a prior reconcile already
// continued the migration but crashed before persisting phase=Running, blindly
// re-issuing ContinueMigration would fail and — with the old logic — cancel a
// perfectly healthy, running migration. To keep the transition crash-safe we
// first read the backend phase and only continue when it hasn't advanced yet.
// Per-object reconcile serialization (a key is processed by at most one worker at
// a time) makes the read-then-continue window race-free within the operator.
func (r *VolumeMigrationReconciler) performMigration(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Re-derive the nodes that need paths, right before the point of no return. The
	// control plane lets a new volume join the subsystem until the migration is
	// activated, and a consumer pod can be rescheduled while we validate — either way
	// a node can appear that was not validated. Validate it too before cutting over.
	if res, wait, err := r.validateLateNodes(ctx, vm); err != nil {
		return res, err
	} else if wait {
		return res, nil
	}

	m, err := r.apiClient.GetMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID)
	if err != nil {
		// Transient read failure: requeue without failing or cancelling.
		log.Error(err, "Cannot read migration before continue; requeuing", "migration", vm.Status.MigrationUUID)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	switch {
	case webapi.MigrationIsTerminal(m.Status):
		// The migration reached a terminal state out-of-band. Do not cancel or
		// re-continue; advance to Running and let reconcileRunning classify it.
		log.Info("Migration already terminal before continue; advancing to Running for classification",
			"migration", vm.Status.MigrationUUID, "status", m.Status)
	case m.Phase == webapi.MigrationPhasePreCreated:
		if err := r.apiClient.ContinueMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID); err != nil {
			// The continue may have taken effect despite the error. Only a
			// migration still stuck in pre_created is a genuine start failure
			// worth cancelling; anything else means it already advanced.
			if m2, gerr := r.apiClient.GetMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID); gerr == nil && m2.Phase == webapi.MigrationPhasePreCreated {
				// Best-effort: the CR fails either way, but a failed cancel leaves
				// target-side objects behind, so it must not be silent.
				if cerr := r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID); cerr != nil {
					log.Error(cerr, "Cannot cancel migration that failed to continue; target-side objects may remain",
						"migration", vm.Status.MigrationUUID, "subsystem", vm.Status.SubsystemNQN)
				}
				return r.setFailed(ctx, vm, fmt.Sprintf("ContinueMigration: %v", err))
			}
			log.Info("ContinueMigration errored but migration has advanced past pre_created; treating as continued",
				"migration", vm.Status.MigrationUUID, "error", err.Error())
		}
	default:
		// Already past pre_created: a prior reconcile continued the migration but
		// did not persist Running. Skip the (now-invalid) continue call.
		log.Info("Migration already continued (past pre_created); skipping ContinueMigration",
			"migration", vm.Status.MigrationUUID, "phase", m.Phase)
	}

	// The validation Jobs have served their purpose and their logs are already in the
	// operator log; reap them as the migration leaves the Validating phase.
	r.deleteValidationJobs(ctx, vm)

	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseRunning
	vm.Status.Connections = nil
	vm.Status.ValidationJobs = nil
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Running: %w", err)
	}

	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationStarted", "MigrationStarted",
		"Migration %s started: volume %s → node %s",
		vm.Status.MigrationUUID, vm.Status.VolumeUUID, vm.Spec.TargetNodeUUID)
	return ctrl.Result{RequeueAfter: vmigration.MigrationInitialDelay}, nil
}

// buildValidationJob constructs the Job that connects NVMe paths and validates
// ANA state on the target node using the simplyblock-rebalancer binary.
func (r *VolumeMigrationReconciler) buildValidationJob(
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
func (r *VolumeMigrationReconciler) buildReleaseJob(
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
			Name:      js.namePrefix + safeNodeID(vm.Status.MigrationUUID) + "-" + nodeSuffix(hostname),
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

// nodeSuffix produces a DNS-label-safe, collision-resistant suffix for a node name.
// Node names can be long FQDNs and are not label-safe, so the short host part is kept
// for readability and a hash of the full name for uniqueness.
func nodeSuffix(nodeName string) string {
	sum := sha256.Sum256([]byte(nodeName))
	short := strings.ToLower(strings.SplitN(nodeName, ".", 2)[0])
	short = nonLabelChars.ReplaceAllString(short, "")
	if len(short) > 16 {
		short = short[:16]
	}
	if short == "" {
		return hex.EncodeToString(sum[:6])
	}
	return short + "-" + hex.EncodeToString(sum[:4])
}

// nonLabelChars matches everything not allowed inside a DNS-1123 label.
var nonLabelChars = regexp.MustCompile(`[^a-z0-9-]`)

// connectionsToValidation converts MigrationConnection status entries to the
// vmigration.Connection type consumed by the simplyblock-rebalancer validate-migration mode.
func connectionsToValidation(conns []simplyblockv1alpha1.MigrationConnection) []vmigration.Connection {
	out := make([]vmigration.Connection, len(conns))
	for i, c := range conns {
		out[i] = vmigration.Connection{
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

// safeNodeID produces a DNS-label-safe suffix from a node UUID.
func safeNodeID(nodeUUID string) string {
	s := strings.ReplaceAll(nodeUUID, "-", "")
	if len(s) > 20 {
		s = s[:20]
	}
	return s
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
func (r *VolumeMigrationReconciler) resolveValidationNodes(
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
func (r *VolumeMigrationReconciler) pvNamesForVolumes(
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

func (r *VolumeMigrationReconciler) resolveConsumerNodeName(
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
func (r *VolumeMigrationReconciler) collectAndLogJobPodLogs(ctx context.Context, job *batchv1.Job) {
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

// defaultRebalancerImage is used when a StorageCluster enables volume migration
// (explicitly, or by default via an omitted settings block) without pinning a
// specific rebalancer image. The image must include nvme-cli.
const defaultRebalancerImage = "docker.io/simplyblock/simplyblock-rebalancer:main"

// resolveRebalancerImage returns the simplyblock-rebalancer image for the StorageCluster
// that owns the migration's volume. Volume migration is enabled by default: an omitted
// VolumeMigrationSettings block (or one without an image) resolves to defaultRebalancerImage;
// only an explicit Enabled=false disables it.
func (r *VolumeMigrationReconciler) resolveRebalancerImage(
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
			return defaultRebalancerImage, nil
		}
		if vm.Enabled != nil && !*vm.Enabled {
			return "", fmt.Errorf("volume migration is disabled for cluster UUID %q", clusterUUID)
		}
		if vm.RebalancerImage != nil && *vm.RebalancerImage != "" {
			return *vm.RebalancerImage, nil
		}
		// Enabled (explicitly or by default) but no image pinned: use the default image.
		return defaultRebalancerImage, nil
	}
	return "", fmt.Errorf("no StorageCluster found for cluster UUID %q", clusterUUID)
}

// reconcileRunning polls the migration API and updates progress in status.
func (r *VolumeMigrationReconciler) reconcileRunning(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// StartedAt is an optional pointer and may be nil on older objects, manual
	// edits, or partial status writes. Backfill it rather than dereferencing a
	// nil pointer (panic) or defaulting to the zero time (instant "stuck" warning).
	if vm.Status.StartedAt == nil {
		now := metav1.Now()
		patch := client.MergeFrom(vm.DeepCopy())
		vm.Status.StartedAt = &now
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("backfill StartedAt: %w", err)
		}
		log.Info("StartedAt was unset in Running phase; backfilled", "migration", vm.Status.MigrationUUID)
	}

	migrationStart := vm.Status.StartedAt.Time
	result, err := vmigration.PollMigration(ctx, r.apiClient, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID, migrationStart)
	if err != nil {
		log.Error(err, "Cannot poll migration; requeuing", "migration", vm.Status.MigrationUUID)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if result.Migration != nil {
		// Update progress fields even if not done yet.
		patch := client.MergeFrom(vm.DeepCopy())
		vm.Status.SourceNodeUUID = result.Migration.SourceNodeID
		vm.Status.MemberCount = result.Migration.MemberCount
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("patch progress: %w", err)
		}
	}

	if result.Stuck {
		r.Recorder.Eventf(vm, nil, corev1.EventTypeWarning, "MigrationStuck", "MigrationStuck",
			"Migration %s has not completed after 30 minutes (phase: %s, status: %s)",
			vm.Status.MigrationUUID, result.Migration.Phase, result.Migration.Status)
	}

	if !result.Done {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Migration finished.
	now := metav1.Now()
	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.CompletedAt = &now
	if result.Succeeded {
		vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseCompleted
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch status Completed: %w", err)
		}
		r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationCompleted", "MigrationCompleted",
			"Migration %s completed successfully", vm.Status.MigrationUUID)
		// A volume moved: flag the owning cluster so the rebalancer's periodic loop
		// triggers a control-plane data realignment. Best-effort — the flag is
		// re-asserted on every completed migration and realignment is idempotent.
		r.markClusterPendingRealignment(ctx, vm.Namespace, vm.Status.ClusterUUID)
	} else {
		vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseFailed
		vm.Status.ErrorMessage = result.Migration.ErrorMessage
		if err := r.Status().Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch status Failed: %w", err)
		}
		r.Recorder.Eventf(vm, nil, corev1.EventTypeWarning, "MigrationFailed", "MigrationFailed",
			"Migration %s failed: %s", vm.Status.MigrationUUID, result.Migration.ErrorMessage)
	}
	return ctrl.Result{}, nil
}

// markClusterPendingRealignment sets status.pendingDataRealignment=true on the
// StorageCluster whose backend UUID matches clusterUUID, recording that a volume has
// moved and a control-plane data realignment is due. The persisted flag is picked up
// by the VolumeRebalancerReconciler's periodic loop.
//
// Best-effort: any failure is logged but does not fail the migration. The flag is a
// monotonic "something moved" marker — it is re-asserted by every completed migration
// and only ever cleared by a successful realignment — so a missed write is corrected
// by the next completing migration.
func (r *VolumeMigrationReconciler) markClusterPendingRealignment(
	ctx context.Context,
	namespace, clusterUUID string,
) {
	log := logf.FromContext(ctx)
	if clusterUUID == "" {
		return
	}
	var clusters simplyblockv1alpha1.StorageClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
		log.Error(err, "Cannot list StorageClusters to flag pending realignment", "clusterUUID", clusterUUID)
		return
	}
	for i := range clusters.Items {
		cr := &clusters.Items[i]
		if cr.Status.UUID != clusterUUID {
			continue
		}
		if ptr.BoolFromOrFalse(cr.Status.PendingDataRealignment) {
			return // already flagged; nothing to do
		}
		patch := client.MergeFrom(cr.DeepCopy())
		cr.Status.PendingDataRealignment = ptr.To(true)
		if err := r.Status().Patch(ctx, cr, patch); err != nil {
			log.Error(err, "Cannot flag StorageCluster pending realignment", "cluster", cr.Name)
		}
		return
	}
	log.Info("No StorageCluster matched volume's cluster UUID; cannot flag pending realignment", "clusterUUID", clusterUUID)
}

// reconcileAbort cancels an in-progress migration.
func (r *VolumeMigrationReconciler) reconcileAbort(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Aborting migration", "migration", vm.Status.MigrationUUID)

	// A migration that was never submitted — still pending, or deferred by a busy
	// cluster — has nothing to cancel on the backend, and calling with an empty id
	// would only produce errors to retry forever.
	if vm.Status.MigrationUUID == "" {
		log.Info("No backend migration was created yet; aborting the request only")
	} else if err := r.apiClient.CancelMigration(ctx, vm.Status.ClusterUUID, vm.Status.SubsystemNQN, vm.Status.MigrationUUID); err != nil {
		log.Error(err, "CancelMigration failed; requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	now := metav1.Now()
	patch := client.MergeFrom(vm.DeepCopy())

	// Best-effort cleanup when aborting during Validating. The validation Jobs go first so
	// that nothing is still connecting paths while the release Jobs look for them — a Job
	// deleted here may still have a pod winding down, so this narrows the race rather than
	// closing it, and what slips through is caught by the reap before the next validation.
	// Both happen before status is cleared, because releasing needs the node list and the
	// connections it is about to drop.
	if len(vm.Status.ValidationJobs) > 0 {
		r.deleteValidationJobs(ctx, vm)
		r.releaseMigrationPaths(ctx, vm)
		vm.Status.ValidationJobs = nil
		vm.Status.Connections = nil
	}

	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseAborted
	vm.Status.CompletedAt = &now
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Aborted: %w", err)
	}
	r.Recorder.Eventf(vm, nil, corev1.EventTypeNormal, "MigrationAborted", "MigrationAborted",
		"Migration %s cancelled", vm.Status.MigrationUUID)
	return ctrl.Result{}, nil
}

// setFailed transitions the migration to Failed with the given reason.
func (r *VolumeMigrationReconciler) setFailed(
	ctx context.Context,
	vm *simplyblockv1alpha1.VolumeMigration,
	reason string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(vm.DeepCopy())
	vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhaseFailed
	vm.Status.ErrorMessage = reason
	if err := r.Status().Patch(ctx, vm, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status Failed: %w", err)
	}
	r.Recorder.Eventf(vm, nil, corev1.EventTypeWarning, "MigrationFailed", "MigrationFailed", "%s", reason)
	return ctrl.Result{}, nil
}

func (r *VolumeMigrationReconciler) SetupWithManager(
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
