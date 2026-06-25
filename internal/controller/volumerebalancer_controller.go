package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration/autobalancing"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

	// rebalancerLabel marks VolumeMigration CRs created by the auto-rebalancer.
	rebalancerLabel = "storage.simplyblock.io/rebalancer"
	// rebalancerClusterLabel records the owning StorageCluster name.
	rebalancerClusterLabel = "storage.simplyblock.io/cluster"
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
	Recorder  record.EventRecorder
	apiClient *webapi.Client

	// LatencyPercentile is the operator-wide fio write-latency percentile ("p50" or
	// "p99") used for the rebalancing deviation signal, set from the --latency-percentile
	// flag. Empty falls back to the config default (p50).
	LatencyPercentile string

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
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=volumemigrations/status,verbs=get
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
	// Apply the operator-wide latency-percentile flag (general, not per cluster).
	if r.LatencyPercentile != "" {
		cfg.LatencyPercentile = r.LatencyPercentile
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

	// Dry-run: when migration creation is disabled the rebalancer still evaluated load and
	// emitted deviation metrics above; we log the candidates it *would* migrate but create
	// no VolumeMigration CRs (e.g. to run workload tests without rebalancer interference).
	if !cfg.MigrationEnabled {
		for _, mc := range toMigrate {
			log.Info("migrationEnabled=false; skipping migration (dry-run)",
				"volume", mc.Volume.UUID, "source", mc.SourceNodeUUID, "target", mc.TargetNodeUUID)
		}
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "dry_run").Inc()
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

// executeMigrations creates a VolumeMigration CR for each MigrationCandidate and
// records cool-down and pending state, returning the number of migrations initiated.
// Source and target are already resolved by the Rebalancer. The VolumeMigration
// controller owns the backend protocol (CreateMigration → validate NVMe paths →
// ContinueMigration → poll); this function only creates the CR and tracks it.
func (r *VolumeRebalancerReconciler) executeMigrations(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	toMigrate []autobalancing.MigrationCandidate,
	coolDownSecs int64,
	cycleDeadline time.Time,
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
		if time.Now().After(cycleDeadline) {
			r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingDeferred",
				"Cycle deadline reached; %d migration candidate(s) deferred to next cycle",
				len(toMigrate)-migratedCount)
			break
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
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingFailed",
				"Creating VolumeMigration for volume %s to node %s failed: %v", mc.Volume.UUID, mc.TargetNodeUUID, err)
			continue
		}
		r.migrationState.PushMigration(mc.ClusterUUID, mc.Volume.PoolUUID, mc.Volume.UUID, name, clusterCR.Namespace, coolDownSecs)
		r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingStarted",
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
				r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingStuck",
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
			r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingComplete",
				"Migration %s of volume %s completed successfully", pm.CRName, volumeUUID)
		} else {
			log.Error(nil, "Volume migration ended without success",
				"volume", volumeUUID, "migration", pm.CRName, "phase", phase, "error", vm.Status.ErrorMessage)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingFailed",
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

func (r *VolumeRebalancerReconciler) SetupWithManager(
	mgr ctrl.Manager,
) error {
	r.apiClient = webapi.NewClient()

	// Index PersistentVolumes by CSI driver so BuildCSIManagedSet can filter to
	// simplyblock CSI volumes through the cache instead of listing every PV.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.PersistentVolume{},
		autobalancing.PVCSIDriverIndexField, func(o client.Object) []string {
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
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&simplyblockv1alpha1.VolumeMigration{}).
		Named("volumerebalancer").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
