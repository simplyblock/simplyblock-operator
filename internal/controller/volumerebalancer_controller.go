package controller

import (
	"context"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration/autobalancing"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
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
	// Defaults applied when the spec field is nil.
	defaultEvaluationInterval = 60 * time.Second
)

// VolumeRebalancerReconciler monitors latency deviation across storage nodes and
// migrates volumes from degraded to healthy nodes.
//
// In-memory state (coolDownMap, pendingMigrations) intentionally does not survive
// operator restarts — the worst-case outcome is one extra migration cycle before
// cool-down re-establishes.
type VolumeRebalancerReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	apiClient *webapi.Client

	migrationState *volumemigration.MigrationState
	rebalancer     *autobalancing.Rebalancer
}

func (r *VolumeRebalancerReconciler) init() {
	r.migrationState = volumemigration.NewMigrationState()
	r.rebalancer = autobalancing.NewRebalancer(
		autobalancing.NewStorageNodeSelector(r.Client),
		autobalancing.NewLogicalVolumeSelector(r.apiClient, r.Client),
	)
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch

func (r *VolumeRebalancerReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	r.init()

	clusterCR := &simplyblockv1alpha1.StorageCluster{}
	if err := r.Get(ctx, req.NamespacedName, clusterCR); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	spec := clusterCR.Spec.VolumeRebalancing
	if spec == nil || (spec.Enabled != nil && !*spec.Enabled) {
		return ctrl.Result{}, nil
	}
	if clusterCR.Status.UUID == "" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	cfg, err := autobalancing.ResolveRebalancingConfig(spec)
	if err != nil {
		log.Error(err, "Invalid rebalancing configuration; skipping cycle")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		return ctrl.Result{RequeueAfter: defaultEvaluationInterval}, nil
	}
	cycleStart := time.Now()

	clusterUUID, err := utils.ResolveClusterUUID(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		log.Error(err, "Cannot get cluster auth; requeuing")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.processPendingMigrations(ctx, clusterCR, clusterUUID)

	nodes, err := r.apiClient.GetStorageNodes(ctx, clusterUUID)
	if err != nil {
		log.Error(err, "Cannot list storage nodes; requeuing")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	nodeMap := make(map[string]webapi.StorageNodeInfo, len(nodes))
	for _, n := range nodes {
		nodeMap[n.UUID] = n
	}

	if hasOfflineNode(nodeMap) {
		log.Info("Cluster has offline node(s); skipping rebalancing cycle")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.EvalInterval)}, nil
	}

	if r.migrationState.HasPendingMigrationForCluster(clusterUUID) {
		log.V(1).Info("Pending migrations exist; deferring new migrations to next cycle")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.EvalInterval)}, nil
	}

	storageNodes := make([]volumemigration.StorageNode, 0, len(nodeMap))
	for uuid := range nodeMap {
		storageNodes = append(storageNodes, volumemigration.StorageNode{UUID: uuid, ClusterUUID: clusterUUID})
	}
	isCoolingDown := func(cUUID, volumeUUID string) bool {
		return r.migrationState.IsVolumeCooledDown(cUUID, volumeUUID, time.Now())
	}

	toMigrate, err := r.rebalancer.SelectMigrations(ctx, cfg, isCoolingDown,
		autobalancing.StorageNodeSelectorInput{
			Namespace:    clusterCR.Namespace,
			StorageNodes: storageNodes,
		})
	if err != nil {
		log.Error(err, "Cannot select migration candidates; requeuing")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if len(toMigrate) == 0 {
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.EvalInterval)}, nil
	}

	if err := r.setRebalancing(ctx, clusterCR, true); err != nil {
		log.Error(err, "Failed to set status.rebalancing=true")
	}
	defer func() {
		if err := r.setRebalancing(ctx, clusterCR, false); err != nil {
			log.Error(err, "Failed to clear status.rebalancing")
		}
	}()

	migratedCount := r.executeMigrations(ctx, clusterCR, toMigrate, cfg.CoolDownSecs, cycleStart.Add(cfg.EvalInterval))

	activeCooldowns := r.migrationState.GetCooldownCountByCluster(clusterUUID, time.Now())
	autobalancing.SetCooldownVolumes(clusterCR.Name, float64(activeCooldowns))

	if migratedCount > 0 {
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "migrated").Inc()
	} else {
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
	}

	return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.EvalInterval)}, nil
}

// executeMigrations submits each MigrationCandidate to the storage API, records
// cool-down and pending state, and returns the number of migrations initiated.
// Source and target are already resolved by the Rebalancer; this function only
// owns API submission and event emission.
func (r *VolumeRebalancerReconciler) executeMigrations(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	toMigrate []autobalancing.MigrationCandidate,
	coolDownSecs int64,
	cycleDeadline time.Time,
) int {
	log := logf.FromContext(ctx)
	migratedCount := 0
	for _, mc := range toMigrate {
		if time.Now().After(cycleDeadline) {
			r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingDeferred",
				"Cycle deadline reached; %d migration candidate(s) deferred to next cycle",
				len(toMigrate)-migratedCount)
			break
		}
		migration, err := r.apiClient.CreateMigration(ctx, mc.ClusterUUID, mc.Volume.PoolUUID, mc.Volume.UUID, mc.TargetNodeUUID)
		if err != nil {
			log.Error(err, "CreateMigration failed", "volume", mc.Volume.UUID, "target", mc.TargetNodeUUID)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingFailed",
				"Migration of volume %s to node %s failed: %v", mc.Volume.UUID, mc.TargetNodeUUID, err)
			continue
		}
		r.migrationState.PushMigration(mc.ClusterUUID, mc.Volume.PoolUUID, mc.Volume.UUID, migration.ID, coolDownSecs)
		r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingStarted",
			"Initiating migration of volume %s from node %s to %s",
			mc.Volume.UUID, mc.SourceNodeUUID, mc.TargetNodeUUID)
		rebalancerMigrationsTotal.WithLabelValues(clusterCR.Name, mc.SourceNodeUUID, mc.TargetNodeUUID).Inc()
		migratedCount++
	}
	return migratedCount
}

// processPendingMigrations polls the migration API for all in-progress migrations
// belonging to the given cluster and removes entries once they complete.
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

		migrationID := pm.MigrationID
		migStart := pm.MigrationStart
		stuckWarned := pm.StuckWarned
		volumeUUID := pm.VolumeUUID

		result, err := volumemigration.PollMigration(ctx, r.apiClient, clusterUUID, pm.PoolUUID, pm.VolumeUUID, migrationID, migStart)
		if err != nil {
			log.Error(err, "Cannot get migration status", "migration", migrationID, "volume", volumeUUID)
			continue
		}

		if result.Stuck && !stuckWarned {
			log.Error(nil, "Volume migration has not completed within 30 minutes",
				"volume", volumeUUID, "migration", migrationID)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingStuck",
				"Migration %s of volume %s has not completed after 30 minutes (phase: %s, status: %s)",
				migrationID, volumeUUID, result.Migration.Phase, result.Migration.Status)

			r.migrationState.MarkMigrationStuck(clusterUUID, volumeUUID)
		}

		if !result.Done {
			continue
		}

		r.migrationState.DeletePendingMigration(clusterUUID, volumeUUID)
		if result.Succeeded {
			log.Info("Volume migration complete", "volume", volumeUUID, "migration", migrationID)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingComplete",
				"Migration %s of volume %s completed successfully", migrationID, volumeUUID)
		} else {
			log.Error(nil, "Volume migration completed with error",
				"volume", volumeUUID, "migration", migrationID, "error", result.Migration.ErrorMessage)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingFailed",
				"Migration %s of volume %s completed with error: %s",
				migrationID, volumeUUID, result.Migration.ErrorMessage)
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

func (r *VolumeRebalancerReconciler) SetupWithManager(
	mgr ctrl.Manager,
) error {
	r.apiClient = webapi.NewClient()
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageCluster{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("volumerebalancer").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
