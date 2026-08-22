package autoplacement

import (
	"context"
	"fmt"
	"time"

	atlaskube "github.com/simplyblock/atlas/kube"
	"github.com/simplyblock/atlas/ptr"
	"github.com/simplyblock/atlas/statemachine"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// rebalancerLabel marks VolumeMigration CRs created by the auto-rebalancer.
	rebalancerLabel = "storage.simplyblock.io/rebalancer"
	// rebalancerClusterLabel records the owning StorageCluster name.
	rebalancerClusterLabel = "storage.simplyblock.io/cluster"

	// defaultDataRealignmentInterval is the spacing between control-plane data
	// realignments when DataRealignment.Interval is unset.
	defaultDataRealignmentInterval = 10 * time.Minute

	// realignmentRetryDelay is how soon to retry after a failed realignment call,
	// rather than waiting a full interval.
	realignmentRetryDelay = 30 * time.Second

	// realignmentBusyRetryDelay is how soon to re-check when a realignment is due but
	// a volume is still moving. Short, because the whole point is to trigger promptly
	// once the cluster goes quiet.
	realignmentBusyRetryDelay = 30 * time.Second

	// defaultDataRealignmentMinMoves is the number of volume moves that must
	// accumulate before a realignment is triggered when MinMoves is unset. One
	// preserves the historical behavior: every completed migration schedules a
	// realignment.
	defaultDataRealignmentMinMoves = 1
)

// rebalanceMigrationName is the deterministic VolumeMigration CR name for a volume.
func rebalanceMigrationName(volumeUUID string) string {
	return "rebalance-" + volumeUUID
}

// VolumeRebalancerReconciler monitors latency deviation across storage nodes and
// migrates volumes from degraded to healthy nodes.
//
// In-memory state (coolDownMap, pendingMigrations) intentionally does not survive
// operator restarts — the worst-case outcome is one extra migration cycle before
// cool-down re-establishes.
type VolumeRebalancerReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	apiClient *webapi.Client

	// LatencyPercentile is the operator-wide fio write-latency percentile ("p50" or
	// "p99") used for the rebalancing deviation signal, set from the --latency-percentile
	// flag. Empty falls back to the config default (p50).
	LatencyPercentile string

	migrationState *volumemigration.MigrationState
	rebalancer     *Rebalancer
}

func (r *VolumeRebalancerReconciler) init(scResolver atlaskube.Resolver) {
	r.migrationState = volumemigration.NewMigrationState()
	r.rebalancer = NewRebalancer(
		NewStorageNodeSelector(r.Client),
		NewLogicalVolumeSelector(r.apiClient, r.Client, scResolver),
	)
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations/status,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get

func (r *VolumeRebalancerReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	clusterCR := &simplyblockv1alpha1.StorageCluster{}
	if err := r.Get(ctx, req.NamespacedName, clusterCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if clusterCR.Status.UUID == "" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Post-migration control-plane data realignment. This runs for every cluster with
	// volume migration enabled, independent of auto-rebalancing, so it also covers
	// manual VolumeMigrations and drain/removal-triggered moves. realignRequeue is the
	// delay until the next realignment check (0 when realignment is disabled).
	//
	// utils.ResolveClusterUUID returns clusterCR.Status.UUID for this cluster; use it
	// directly for the realignment call, which needs no other lookup.
	realignRequeue := r.reconcileDataRealignment(ctx, clusterCR, clusterCR.Status.UUID)

	// Auto-rebalancing is opt-in: run only when explicitly enabled (Enabled=true).
	// An unset flag means off, so realignment still gets its requeue. No cycle runs
	// here, which is why nothing is recorded against the evaluation metric — a cluster
	// that has rebalancing switched off is not a cluster whose cycles are being skipped.
	spec := GetConfig(clusterCR.Spec.VolumeAutoPlacement)
	if !ptr.BoolFromOrFalse(spec.Enabled) {
		return ctrl.Result{RequeueAfter: realignRequeue}, nil
	}

	return r.evaluate(ctx, clusterCR, spec, realignRequeue)
}

// evaluate runs one rebalancing evaluation cycle: read the cluster's load, decide
// whether volumes should move, and create the VolumeMigrations that move them. It
// reports when the next cycle should run.
//
// The cycle is driven as a state machine whose terminal phases are the outcomes a cycle
// can have — see [cyclePhase]. Every route out of here therefore goes through one of
// deferCycle, failCycle, dryRunCycle or the Migrating → Completed pair, and the outcome
// metric is recorded by the phase rather than by the route.
func (r *VolumeRebalancerReconciler) evaluate(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	spec simplyblockv1alpha1.VolumeAutoPlacementSettings,
	realignRequeue time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	cycleStart := time.Now()

	// A configuration that does not parse still spends a cycle and still owes an
	// outcome, so it is reported through the machine like any other. Its own interval
	// is unusable, so the cycle is bounded and paced by the default one.
	cfg, cfgErr := ResolveAutoPlacementConfig(spec)
	interval := DefaultEvaluationInterval
	if cfgErr == nil {
		// The operator-wide latency-percentile flag is general, not per cluster.
		if r.LatencyPercentile != "" {
			cfg.LatencyPercentile = r.LatencyPercentile
		}
		interval = cfg.EvalInterval
	}

	c := &cycle{cluster: clusterCR, deadline: cycleStart.Add(interval)}
	sm := r.cycleMachine(ctx, c)
	defer sm.Close()

	// paced is the requeue for a cycle that ran its course: whatever is left of the
	// evaluation interval, shortened when a realignment check is due sooner.
	paced := nextRequeue(cycleStart, interval, realignRequeue)

	if cfgErr != nil {
		log.Error(cfgErr, "Invalid rebalancing configuration; skipping cycle")
		return r.deferCycle(ctx, c, sm, "invalid rebalancing configuration", DefaultEvaluationInterval)
	}

	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		log.Error(err, "Cannot get cluster auth; requeuing")
		return r.failCycle(ctx, c, sm, "cluster auth unavailable", 30*time.Second)
	}
	c.clusterUUID = clusterUUID

	r.processPendingMigrations(ctx, clusterCR, clusterUUID)

	nodes, err := r.apiClient.GetStorageNodes(ctx, clusterUUID)
	if err != nil {
		log.Error(err, "Cannot list storage nodes; requeuing")
		return r.failCycle(ctx, c, sm, "cannot list storage nodes", 30*time.Second)
	}
	nodeMap := make(map[string]webapi.StorageNodeInfo, len(nodes))
	for _, n := range nodes {
		nodeMap[n.UUID] = n
	}

	if hasOfflineNode(nodeMap) {
		log.Info("Cluster has offline node(s); skipping rebalancing cycle")
		return r.deferCycle(ctx, c, sm, "cluster has offline node(s)", paced)
	}

	if r.migrationState.HasPendingMigrationForCluster(clusterUUID) {
		log.V(1).Info("Pending migrations exist; deferring new migrations to next cycle")
		return r.deferCycle(ctx, c, sm, "migrations already pending", paced)
	}

	storageNodes := make([]volumemigration.StorageNode, 0, len(nodeMap))
	for uuid := range nodeMap {
		storageNodes = append(storageNodes, volumemigration.StorageNode{UUID: uuid, ClusterUUID: clusterUUID})
	}
	isCoolingDown := func(cUUID, volumeUUID string) bool {
		return r.migrationState.IsVolumeCooledDown(cUUID, volumeUUID, time.Now())
	}

	toMigrate, pinnedBlocked, err := r.rebalancer.SelectMigrations(ctx, cfg, isCoolingDown,
		StorageNodeSelectorInput{
			Namespace:    clusterCR.Namespace,
			StorageNodes: storageNodes,
		})
	if err != nil {
		log.Error(err, "Cannot select migration candidates; requeuing")
		return r.failCycle(ctx, c, sm, "cannot select migration candidates", 30*time.Second)
	}
	if pinnedBlocked {
		// A hot node could not be rebalanced because every volume it hosts is pinned.
		log.Info("rebalancing blocked: hot node has only pinned volumes", "cluster", clusterCR.Name)
		rebalancerPinnedBlockedTotal.WithLabelValues(clusterCR.Name).Inc()
	}
	if len(toMigrate) == 0 {
		return r.deferCycle(ctx, c, sm, "no migration candidates", paced)
	}

	// Dry-run: when migration creation is disabled the rebalancer still evaluated load and
	// emitted deviation metrics above; we log the candidates it *would* migrate but create
	// no VolumeMigration CRs (e.g. to run workload tests without rebalancer interference).
	if !cfg.MigrationEnabled {
		for _, mc := range toMigrate {
			log.Info("migrationEnabled=false; skipping migration (dry-run)",
				"volume", mc.Volume.UUID, "source", mc.SourceNodeUUID, "target", mc.TargetNodeUUID)
		}
		return r.dryRunCycle(ctx, c, sm, paced)
	}

	// Past this point the cycle is going to move volumes. Entering Migrating raises
	// status.rebalancing and arms the phase with what is left of the cycle; Completed,
	// its only exit, lowers the flag again and records how many CRs were created.
	if err := sm.TransitionTo(ctx, cycleMigrating); err != nil {
		return ctrl.Result{}, err
	}
	c.migrated = r.executeMigrations(ctx, sm, clusterCR, toMigrate, cfg.CoolDownSecs)
	if err := sm.TransitionTo(ctx, cycleCompleted); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, interval)}, nil
}

// executeMigrations creates a VolumeMigration CR for each MigrationCandidate and
// records cool-down and pending state, returning the number of migrations initiated.
// Source and target are already resolved by the Rebalancer. The VolumeMigration
// controller owns the backend protocol (CreateMigration → validate NVMe paths →
// ContinueMigration → poll); this function only creates the CR and tracks it.
//
// The cycle's remaining time is the Migrating phase's own bound, so the loop stops when
// the machine says the phase is over rather than by comparing clocks. Note which
// context does what: sm.Done() decides whether to start another migration, while the
// creation itself carries ctx — a CR create cancelled halfway may still have taken
// effect, so the boundary is between migrations, never inside one.
func (r *VolumeRebalancerReconciler) executeMigrations(
	ctx context.Context,
	sm *statemachine.Machine[cyclePhase],
	clusterCR *simplyblockv1alpha1.StorageCluster,
	toMigrate []MigrationCandidate,
	coolDownSecs int64,
) int {
	log := logf.FromContext(ctx)
	ownerRefs := []metav1.OwnerReference{{
		APIVersion: simplyblockv1alpha1.GroupVersion.String(),
		Kind:       "StorageCluster",
		Name:       clusterCR.Name,
		UID:        clusterCR.UID,
	}}
	migratedCount := 0
	for _, mc := range toMigrate {
		select {
		case <-sm.Done():
			r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeNormal, "VolumeRebalancingDeferred", "VolumeRebalancingDeferred",
				"Cycle deadline reached; %d migration candidate(s) deferred to next cycle",
				len(toMigrate)-migratedCount)
			return migratedCount
		default:
		}

		name := rebalanceMigrationName(mc.Volume.UUID)
		labels := map[string]string{
			rebalancerLabel:        "true",
			rebalancerClusterLabel: clusterCR.Name,
		}

		err := volumemigration.StartMigration(ctx, r.Client, mc.Volume.UUID, mc.TargetNodeUUID,
			name, clusterCR.Namespace, ownerRefs, labels)
		switch {
		case apierrors.IsAlreadyExists(err):
			// A VolumeMigration for this volume already exists (in flight, or a
			// leftover not yet reaped). Track it and move on rather than duplicating.
			log.Info("VolumeMigration CR already exists; tracking existing", "name", name, "volume", mc.Volume.UUID)
		case err != nil:
			log.Error(err, "Failed to create VolumeMigration CR", "volume", mc.Volume.UUID, "target", mc.TargetNodeUUID)
			r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeWarning, "VolumeRebalancingFailed", "VolumeRebalancingFailed",
				"Creating VolumeMigration for volume %s to node %s failed: %v", mc.Volume.UUID, mc.TargetNodeUUID, err)
			continue
		}

		r.migrationState.PushMigration(mc.ClusterUUID, mc.Volume.PoolUUID, mc.Volume.UUID, name, clusterCR.Namespace, coolDownSecs)
		r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeNormal, "VolumeRebalancingStarted", "VolumeRebalancingStarted",
			"Created VolumeMigration %s for volume %s from node %s to %s",
			name, mc.Volume.UUID, mc.SourceNodeUUID, mc.TargetNodeUUID)
		rebalancerMigrationsTotal.WithLabelValues(clusterCR.Name, mc.SourceNodeUUID, mc.TargetNodeUUID).Inc()
		migratedCount++
	}
	return migratedCount
}

// processPendingMigrations inspects the VolumeMigration CR backing each in-progress
// migration for the cluster and removes the pending entry once the CR reaches a
// terminal phase. The VolumeMigration controller drives the actual backend protocol
// (validate paths + ContinueMigration + poll); this only tracks completion and
// reaps the finished CR.
func (r *VolumeRebalancerReconciler) processPendingMigrations(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	clusterUUID string,
) {
	log := logf.FromContext(ctx)
	prefix := clusterUUID + "/"

	keys := r.migrationState.GetPendingMigrationKeysWithPrefix(prefix)
	for _, key := range keys {
		pm, ok := r.migrationState.GetPendingMigrationByKey(key)
		if !ok {
			continue
		}
		volumeUUID := pm.VolumeUUID

		vm := &simplyblockv1alpha1.VolumeMigration{}
		err := r.Get(ctx, types.NamespacedName{Name: pm.CRName, Namespace: pm.CRNamespace}, vm)
		if apierrors.IsNotFound(err) {
			// CR was deleted out from under us (manual cleanup / GC). Stop tracking.
			log.Info("VolumeMigration CR gone; clearing pending", "name", pm.CRName, "volume", volumeUUID)
			r.migrationState.DeletePendingMigration(clusterUUID, volumeUUID)
			continue
		}
		if err != nil {
			log.Error(err, "Cannot get VolumeMigration CR", "name", pm.CRName, "volume", volumeUUID)
			continue
		}

		phase := vm.Status.Phase
		terminal := phase == simplyblockv1alpha1.VolumeMigrationPhaseCompleted ||
			phase == simplyblockv1alpha1.VolumeMigrationPhaseFailed ||
			phase == simplyblockv1alpha1.VolumeMigrationPhaseAborted
		if !terminal {
			if time.Since(pm.MigrationStart) > volumemigration.MigrationStuckWarningTimeout && !pm.StuckWarned {
				log.Error(nil, "Volume migration has not completed within 30 minutes",
					"volume", volumeUUID, "migration", pm.CRName, "phase", phase)
				r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeWarning, "VolumeRebalancingStuck", "VolumeRebalancingStuck",
					"Migration %s of volume %s has not completed after 30 minutes (phase: %s)",
					pm.CRName, volumeUUID, phase)
				r.migrationState.MarkMigrationStuck(clusterUUID, volumeUUID)
			}
			continue
		}

		// Terminal: record outcome, reap the CR, stop tracking.
		r.migrationState.DeletePendingMigration(clusterUUID, volumeUUID)
		if phase == simplyblockv1alpha1.VolumeMigrationPhaseCompleted {
			log.Info("Volume migration complete", "volume", volumeUUID, "migration", pm.CRName)
			r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeNormal, "VolumeRebalancingComplete", "VolumeRebalancingComplete",
				"Migration %s of volume %s completed successfully", pm.CRName, volumeUUID)
		} else {
			log.Error(nil, "Volume migration ended without success",
				"volume", volumeUUID, "migration", pm.CRName, "phase", phase, "error", vm.Status.ErrorMessage)
			r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeWarning, "VolumeRebalancingFailed", "VolumeRebalancingFailed",
				"Migration %s of volume %s ended in phase %s: %s",
				pm.CRName, volumeUUID, phase, vm.Status.ErrorMessage)
		}
		if err := r.Delete(ctx, vm); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete completed VolumeMigration CR", "name", pm.CRName)
		}
	}
}

// setRebalancing patches status.rebalancing on the StorageCluster CR.
func (r *VolumeRebalancerReconciler) setRebalancing(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	value bool,
) error {
	orig := clusterCR.DeepCopy()
	clusterCR.Status.Rebalancing = &value
	return r.Status().Patch(ctx, clusterCR, client.MergeFrom(orig))
}

// hasOfflineNode returns true if any node in the map is not online or is unreachable.
func hasOfflineNode(
	nodeMap map[string]webapi.StorageNodeInfo,
) bool {
	for _, n := range nodeMap {
		switch n.Status {
		case "offline", "in_restart", "unreachable":
			return true
		}
	}
	return false
}

// requeueAfter returns the time remaining until the next evaluation, clamped to 0.
func requeueAfter(
	cycleStart time.Time,
	EvalInterval time.Duration,
) time.Duration {
	remaining := EvalInterval - time.Since(cycleStart)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// nextRequeue returns the auto-rebalancing requeue delay, shortened to the
// data-realignment interval when the latter is sooner. This keeps realignment on
// schedule even if the rebalancing evaluation interval is configured longer than the
// realignment interval. A realignRequeue of 0 (realignment disabled) is ignored.
func nextRequeue(
	cycleStart time.Time,
	evalInterval, realignRequeue time.Duration,
) time.Duration {
	d := requeueAfter(cycleStart, evalInterval)
	if realignRequeue > 0 && realignRequeue < d {
		return realignRequeue
	}
	return d
}

// reconcileDataRealignment triggers a control-plane data realignment when one is due
// and returns the delay until the next realignment check (0 when realignment is
// disabled for the cluster).
//
// A realignment runs when either:
//   - the TriggerRealignmentAnnotation is set to a non-empty value (explicit,
//     immediate trigger — e.g. after a storage node drain and removal), which
//     bypasses every gate below; or
//   - at least MinMoves volume moves are outstanding (status.volumeMoveGeneration
//     beyond status.realignedGeneration) AND at least Interval has elapsed since the
//     last successful realignment AND no volume is currently moving.
//
// On success the covered generation is recorded and lastDataRealignmentAt is stamped,
// so the next interval window starts fresh and a realignment never runs with nothing
// to align.
func (r *VolumeRebalancerReconciler) reconcileDataRealignment(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	clusterUUID string,
) time.Duration {
	log := logf.FromContext(ctx)

	enabled, interval, minMoves := resolveDataRealignmentConfig(clusterCR)
	if !enabled {
		return 0
	}

	// Any non-empty annotation value forces an immediate run; an empty value (or an
	// absent annotation) does not.
	forced := clusterCR.Annotations[simplyblockv1alpha1.TriggerRealignmentAnnotation] != ""
	// The generation read here is what a successful request will be recorded as
	// covering, so it must be taken before the call and not re-read after it.
	generation := ptr.Int64FromOrZero(clusterCR.Status.VolumeMoveGeneration)
	outstanding := generation - ptr.Int64FromOrZero(clusterCR.Status.RealignedGeneration)

	if !forced && outstanding < minMoves {
		// Not enough has moved yet — re-check at the interval boundary.
		return interval
	}

	// Honor interval spacing for the periodic (non-forced) path. Explicit triggers
	// run immediately regardless of when the last realignment happened. Checked
	// before the in-flight lookup below, which costs a List.
	if !forced && clusterCR.Status.LastDataRealignmentAt != nil {
		if elapsed := time.Since(clusterCR.Status.LastDataRealignmentAt.Time); elapsed < interval {
			return interval - elapsed
		}
	}

	// Never realign while a volume is still moving. Two reasons, and the second is the
	// one that bites: realigning to "current placement" while placement is still
	// changing asks the control plane to chase a moving target, and a migration that
	// completes after the request went out raises the generation past what the request
	// covers — so the next interval tick fires a second realignment on top of the one
	// still running. Waiting for quiescence removes both, and because the control plane
	// refuses new migrations while a realignment runs, nothing can then complete
	// mid-realignment to re-arm it.
	//
	// Forced triggers skip this: a drain or node removal has decided the realignment is
	// needed now, and deferring it could leave a removed node's data unaligned.
	if !forced {
		moving, err := r.movingVolumes(ctx, clusterCR)
		if err != nil {
			log.Error(err, "Cannot list VolumeMigrations; deferring realignment", "cluster", clusterCR.Name)
			return realignmentBusyRetryDelay
		}
		if len(moving) > 0 {
			log.V(1).Info("Deferring data realignment; volume(s) still moving",
				"cluster", clusterCR.Name, "migrations", moving, "outstandingMoves", outstanding)
			return realignmentBusyRetryDelay
		}
	}

	if err := r.apiClient.TriggerDataRealignment(ctx, clusterUUID); err != nil {
		log.Error(err, "Control-plane data realignment failed; will retry", "cluster", clusterCR.Name)
		r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeWarning, "DataRealignmentFailed", "DataRealignmentFailed",
			"Control-plane data realignment failed: %v", err)
		return realignmentRetryDelay
	}

	// Success — record the generation this realignment covers and stamp the time so the
	// interval restarts.
	now := metav1.Now()
	patch := client.MergeFrom(clusterCR.DeepCopy())
	clusterCR.Status.RealignedGeneration = ptr.To(generation)
	clusterCR.Status.LastDataRealignmentAt = &now
	if err := r.Status().Patch(ctx, clusterCR, patch); err != nil {
		// The realignment happened; failing to record it only risks one extra
		// (idempotent) realignment next cycle. Log and continue.
		log.Error(err, "Realignment triggered but recording the covered generation failed",
			"cluster", clusterCR.Name, "generation", generation)
	}

	if forced {
		if err := r.removeTriggerAnnotation(ctx, clusterCR); err != nil {
			log.Error(err, "Cannot remove realignment trigger annotation", "cluster", clusterCR.Name)
		}
	}

	log.Info("Control-plane data realignment triggered", "cluster", clusterCR.Name,
		"forced", forced, "moves", outstanding, "generation", generation)
	r.Recorder.Eventf(clusterCR, nil, corev1.EventTypeNormal, "DataRealignmentTriggered", "DataRealignmentTriggered",
		"Control-plane data realignment triggered after %d volume move(s) to re-align data structures to current volume placement",
		outstanding)
	return interval
}

// movingVolumes names the VolumeMigrations for this cluster that the control plane has
// accepted and not yet finished, i.e. the ones that may be moving data right now.
//
// Deliberately keyed on MigrationUUID rather than phase alone. A CR whose submission
// was refused — which is exactly what happens while a realignment is running, since the
// control plane rejects migrations then — sits non-terminal for as long as it keeps
// retrying, but no data is moving and it must not hold realignment off. Only a
// migration the control plane has taken on counts.
func (r *VolumeRebalancerReconciler) movingVolumes(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) ([]string, error) {
	var migrations simplyblockv1alpha1.VolumeMigrationList
	if err := r.List(ctx, &migrations, client.InNamespace(clusterCR.Namespace)); err != nil {
		return nil, fmt.Errorf("list VolumeMigrations in %s: %w", clusterCR.Namespace, err)
	}
	var moving []string
	for i := range migrations.Items {
		vm := &migrations.Items[i]
		if vm.Status.MigrationUUID == "" {
			continue // never accepted by the control plane; nothing is moving
		}
		if vm.Status.ClusterUUID != "" && vm.Status.ClusterUUID != clusterCR.Status.UUID {
			continue // another cluster in the same namespace
		}
		switch vm.Status.Phase {
		case simplyblockv1alpha1.VolumeMigrationPhaseCompleted,
			simplyblockv1alpha1.VolumeMigrationPhaseFailed,
			simplyblockv1alpha1.VolumeMigrationPhaseAborted:
			continue
		}
		moving = append(moving, vm.Name)
	}
	return moving, nil
}

// removeTriggerAnnotation deletes the explicit-trigger annotation from the cluster,
// so a one-shot force is consumed exactly once.
func (r *VolumeRebalancerReconciler) removeTriggerAnnotation(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
) error {
	if _, ok := clusterCR.Annotations[simplyblockv1alpha1.TriggerRealignmentAnnotation]; !ok {
		return nil
	}
	patch := client.MergeFrom(clusterCR.DeepCopy())
	delete(clusterCR.Annotations, simplyblockv1alpha1.TriggerRealignmentAnnotation)
	return r.Patch(ctx, clusterCR, patch)
}

// resolveDataRealignmentConfig reports whether post-migration data realignment is
// enabled for the cluster and the interval between realignments. Realignment is on by
// default and is only meaningful while volume migration itself is enabled.
func resolveDataRealignmentConfig(
	clusterCR *simplyblockv1alpha1.StorageCluster,
) (enabled bool, interval time.Duration, minMoves int64) {
	interval = defaultDataRealignmentInterval
	minMoves = defaultDataRealignmentMinMoves
	vms := clusterCR.Spec.VolumeMigrationSettings
	if vms != nil && !ptr.BoolFromOrTrue(vms.Enabled) {
		// Volume migration disabled — nothing ever moves, so nothing to realign.
		return false, interval, minMoves
	}
	if vms == nil || vms.DataRealignment == nil {
		return true, interval, minMoves
	}
	dr := vms.DataRealignment
	if !ptr.BoolFromOrTrue(dr.Enabled) {
		return false, interval, minMoves
	}
	if dr.Interval != nil && dr.Interval.Duration > 0 {
		interval = dr.Interval.Duration
	}
	if n := ptr.Int64From(dr.MinMoves, defaultDataRealignmentMinMoves); n > 0 {
		minMoves = n
	}
	return true, interval, minMoves
}

func (r *VolumeRebalancerReconciler) SetupWithManager(
	mgr ctrl.Manager,
) error {
	r.apiClient = webapi.NewClient()

	// A client-go clientset backs the kube.LiveResolver used to read the
	// StorageClass of each candidate volume (see BuildNamespacedSet). StorageClass
	// reads are rare and off the hot path, so direct API reads are preferable to
	// making the manager cache watch every StorageClass in the cluster.
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("build clientset for StorageClass resolver: %w", err)
	}

	// Initialize in-memory migration state and the rebalancer once, here.
	// Re-initializing in Reconcile would wipe coolDownMap/pendingMigrations,
	// defeating cool-down and pending-migration tracking.
	r.init(atlaskube.NewLiveResolver(clientset))

	// Index PersistentVolumes by CSI driver so BuildCSIManagedSet can filter to
	// simplyblock CSI volumes through the cache instead of listing every PV.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.PersistentVolume{},
		PVCSIDriverIndexField, func(o client.Object) []string {
			pv, ok := o.(*corev1.PersistentVolume)
			if !ok || pv.Spec.CSI == nil {
				return nil
			}
			return []string{pv.Spec.CSI.Driver}
		}); err != nil {
		return fmt.Errorf("index PersistentVolumes by CSI driver: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageCluster{},
			// React to spec changes (generation) and to annotation changes so an
			// explicit realignment trigger (TriggerRealignmentAnnotation) reconciles
			// the cluster immediately rather than waiting for the next interval tick.
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			))).
		Owns(&simplyblockv1alpha1.VolumeMigration{}).
		Named("volumerebalancer").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
