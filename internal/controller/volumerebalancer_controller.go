package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration/autobalancing"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	promlatency "github.com/simplyblock/simplyblock-operator/internal/metrics/prometheus"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	// pinnedVolumeAnnotation is checked on the PVC; any non-empty value pins the volume.
	pinnedVolumeAnnotation = "simplyblock.io/pinned-volume"

	// Defaults applied when the spec field is nil.
	defaultEvaluationInterval = 60 * time.Second

	// migrationBudgetFraction is the fraction of the source node's total volume IO score
	// that may be migrated in a single evaluation cycle.
	migrationBudgetFraction = 0.10
)

// nodeLatencyData holds the fio p99 latency measurements for one storage node.
type nodeLatencyData struct {
	baselineP99NS int64
	currentP99NS  int64
}

// volumePlacement associates a VolumeInfo with the pool it belongs to.
type volumePlacement struct {
	webapi.VolumeInfo
	poolUUID string
}

// rankedCandidate pairs a migration-eligible volume with its IO score and pool UUID.
type rankedCandidate struct {
	vol   webapi.VolumeInfo
	score float64
	pool  string
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

	now := time.Now()

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

	deviations, maxDev, avgDev, hottestNode, coolestNode, err := r.computeLatencyDeviations(ctx, clusterCR, clusterUUID, cfg.PrometheusURL)
	if err != nil {
		if errors.Is(err, promlatency.ErrLatencyDataNotReady) {
			log.Info("Latency data not yet available; waiting for fio-bench-probe sidecar baseline")
			rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		} else {
			log.Info("Cannot collect latency from Prometheus; requeuing", "error", err)
			rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "error").Inc()
		}
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.EvalInterval)}, nil
	}

	hotNodes := volumemigration.NodesAboveThreshold(deviations, cfg.ImbalanceThreshold)
	if len(hotNodes) == 0 {
		log.V(1).Info("No node exceeds latency deviation threshold; skipping",
			"maxDeviationPct", maxDev, "threshold", cfg.ImbalanceThreshold)
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		r.patchRebalancingMetrics(ctx, clusterCR, deviations, nil, maxDev, avgDev, hottestNode, coolestNode, now)
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.EvalInterval)}, nil
	}

	if r.migrationState.HasPendingMigrationForCluster(clusterUUID) {
		log.V(1).Info("Pending migrations exist; deferring new migrations to next cycle")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		r.patchRebalancingMetrics(ctx, clusterCR, deviations, nil, maxDev, avgDev, hottestNode, coolestNode, now)
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.EvalInterval)}, nil
	}

	// Build the input for the rebalancer from the nodes already fetched above.
	storageNodes := make([]volumemigration.StorageNode, 0, len(nodeMap))
	for uuid := range nodeMap {
		storageNodes = append(storageNodes, volumemigration.StorageNode{UUID: uuid, ClusterUUID: clusterUUID})
	}
	selectorInput := autobalancing.StorageNodeSelectorInput{
		Namespace:    clusterCR.Namespace,
		StorageNodes: storageNodes,
	}

	isCoolingDown := func(cUUID, volumeUUID string) bool {
		return r.migrationState.IsVolumeCooledDown(cUUID, volumeUUID, time.Now())
	}

	toMigrate, err := r.rebalancer.SelectMigrations(ctx, cfg, isCoolingDown, selectorInput)
	if err != nil {
		log.Error(err, "Cannot select migration candidates; requeuing")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if len(toMigrate) == 0 {
		log.Info("All latency-hot nodes have no eligible migration candidates (pinned or in cool-down)")
		r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingBlocked",
			"All %d latency-degraded node(s) have no eligible migration candidates. Pinned volumes or active cool-downs are preventing rebalancing.",
			len(hotNodes))
		rebalancerPinnedBlockedTotal.WithLabelValues(clusterCR.Name).Inc()
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "blocked").Inc()
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

	migratedCount := r.executeMigrations(ctx, clusterCR, toMigrate, deviations, cfg.CoolDownSecs, cycleStart.Add(cfg.EvalInterval))

	r.patchRebalancingMetrics(ctx, clusterCR, deviations, nil, maxDev, avgDev, hottestNode, coolestNode, now)

	activeCooldowns := r.migrationState.GetCooldownCountByCluster(clusterUUID, now)
	autobalancing.SetCooldownVolumes(clusterCR.Name, float64(activeCooldowns))

	if migratedCount > 0 {
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "migrated").Inc()
	} else {
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
	}

	return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.EvalInterval)}, nil
}

// computeLatencyDeviations collects per-node latency from Prometheus and StorageNode CRs,
// computes deviation from baseline, emits Prometheus gauges, and returns the deviation map
// plus cluster-level statistics.
func (r *VolumeRebalancerReconciler) computeLatencyDeviations(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	clusterUUID, PrometheusURL string,
) (deviations map[string]float64, maxDev, avgDev float64, hottestNode, coolestNode string, err error) {
	latencyByNode, err := r.collectLatencyState(ctx, clusterCR.Namespace, clusterUUID, PrometheusURL)
	if err != nil {
		return nil, 0, 0, "", "", err
	}
	deviations = make(map[string]float64, len(latencyByNode))
	for nodeUUID, ld := range latencyByNode {
		deviations[nodeUUID] = volumemigration.ComputeLatencyDeviationPct(ld.baselineP99NS, ld.currentP99NS)
	}
	// Gauges (rebalancerNodeLatencyDeviationPct, rebalancerMaxLatencyDeviationPct)
	// are emitted by StorageNodeSelector inside the Rebalancer; no duplicate update here.
	maxDev, avgDev, hottestNode, coolestNode = deviationStats(deviations)
	return deviations, maxDev, avgDev, hottestNode, coolestNode, nil
}

// collectAndEnrichVolumes lists all volumes per node and overlays IOPS/throughput from
// Prometheus. On Prometheus failure it falls back to REST API values (may be zero).
func (r *VolumeRebalancerReconciler) collectAndEnrichVolumes(
	ctx context.Context,
	clusterUUID, PrometheusURL string,
) (volumesByNode map[string][]webapi.VolumeInfo, allVolumes map[string]volumePlacement, err error) {
	log := logf.FromContext(ctx)
	volumesByNode, allVolumes, _, err = r.collectVolumesByNode(ctx, clusterUUID)
	if err != nil {
		return nil, nil, err
	}
	if ioProvider, pErr := promlatency.New(PrometheusURL); pErr != nil {
		log.Error(pErr, "Cannot create volume IO provider; scoring will use REST API values")
	} else if prometheusVolumeIO, pErr := ioProvider.GetClusterVolumeIO(ctx, clusterUUID); pErr != nil {
		log.Error(pErr, "Cannot query volume IO from Prometheus; scoring will use REST API values")
	} else {
		overrideVolumeIO(volumesByNode, prometheusVolumeIO)
	}
	return volumesByNode, allVolumes, nil
}

// selectSourceNode iterates hot nodes (worst-first) and returns the first that has at
// least one eligible migration candidate, along with those candidates ranked by IO score.
// Returns empty string and nil slice when no source can be found.
func (r *VolumeRebalancerReconciler) selectSourceNode(
	hotNodes []string,
	volumesByNode map[string][]webapi.VolumeInfo,
	allVolumes map[string]volumePlacement,
	clusterUUID string,
	pinnedVolumeUUIDs map[string]bool,
	IopsWeight, ThroughputWeight float64,
) (sourceNodeUUID string, candidates []rankedCandidate) {
	for _, nodeUUID := range hotNodes {
		eligible := r.filterEligibleVolumes(volumesByNode[nodeUUID], clusterUUID, pinnedVolumeUUIDs)
		if len(eligible) == 0 {
			continue
		}
		ranked := make([]rankedCandidate, 0, len(eligible))
		for _, vol := range eligible {
			cs := volumemigration.VolumeIOScore(vol.IOPS, vol.ThroughputBytesPerSec, IopsWeight, ThroughputWeight)
			ranked = append(ranked, rankedCandidate{vol: vol, score: cs, pool: allVolumes[vol.UUID].poolUUID})
		}
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
		return nodeUUID, ranked
	}
	return "", nil
}

// selectMigrationSet selects at most MaxMigrations candidates from the ranked list,
// capped by a 10% IO-budget fraction of the total candidate score.
func selectMigrationSet(
	candidates []rankedCandidate,
	MaxMigrations int,
) []rankedCandidate {
	var totalScore float64
	for _, rc := range candidates {
		totalScore += rc.score
	}
	budget := migrationBudgetFraction * totalScore
	toMigrate := make([]rankedCandidate, 0, MaxMigrations)
	for _, rc := range candidates {
		if len(toMigrate) == 0 || rc.score <= budget {
			toMigrate = append(toMigrate, rc)
			budget -= rc.score
		}
		if len(toMigrate) >= MaxMigrations {
			break
		}
	}
	return toMigrate
}

// executeMigrations calls CreateMigration for each candidate, tracks cool-down and
// pending state, and returns the number of migrations successfully initiated.
// executeMigrations submits each MigrationCandidate to the storage API, records
// cool-down and pending state, and returns the number of migrations initiated.
// Source and target are already resolved by the Rebalancer; this function only
// owns API submission and event emission.
func (r *VolumeRebalancerReconciler) executeMigrations(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	toMigrate []autobalancing.MigrationCandidate,
	deviations map[string]float64,
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
		migration, err := r.apiClient.CreateMigration(ctx, mc.ClusterUUID, mc.Volume.UUID, mc.TargetNodeUUID)
		if err != nil {
			log.Error(err, "CreateMigration failed", "volume", mc.Volume.UUID, "target", mc.TargetNodeUUID)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingFailed",
				"Migration of volume %s to node %s failed: %v", mc.Volume.UUID, mc.TargetNodeUUID, err)
			continue
		}
		r.migrationState.PushMigration(mc.ClusterUUID, mc.Volume.PoolUUID, mc.Volume.UUID, migration.ID, coolDownSecs)
		r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingStarted",
			"Initiating migration of volume %s from node %s to %s (latency deviation: %.1f%%)",
			mc.Volume.UUID, mc.SourceNodeUUID, mc.TargetNodeUUID, deviations[mc.SourceNodeUUID])
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

		result, err := volumemigration.PollMigration(ctx, r.apiClient, clusterUUID, migrationID, migStart)
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

// collectVolumesByNode lists all volumes across all pools and returns:
// - volumesByNode: nodeUUID → []VolumeInfo
// - allVolumes: volumeUUID → {VolumeInfo, poolUUID}
// - clusterAvgUsedBytes: mean used bytes across all volumes
func (r *VolumeRebalancerReconciler) collectVolumesByNode(
	ctx context.Context,
	clusterUUID string,
) (map[string][]webapi.VolumeInfo, map[string]volumePlacement, int64, error) {
	pools, err := r.apiClient.GetStoragePools(ctx, clusterUUID)
	if err != nil {
		return nil, nil, 0, err
	}

	volumesByNode := make(map[string][]webapi.VolumeInfo)
	allVolumes := make(map[string]volumePlacement)

	var totalUsed int64
	var volumeCount int64

	for _, pool := range pools {
		vols, err := r.apiClient.GetPoolVolumes(ctx, clusterUUID, pool.UUID)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("pool %s: %w", pool.UUID, err)
		}
		for _, v := range vols {
			volumesByNode[v.PrimaryNodeUUID] = append(volumesByNode[v.PrimaryNodeUUID], v)
			allVolumes[v.UUID] = volumePlacement{v, pool.UUID}
			totalUsed += v.Capacity.SizeUsed
			volumeCount++
		}
	}

	avgUsed := int64(0)
	if volumeCount > 0 {
		avgUsed = totalUsed / volumeCount
	}
	return volumesByNode, allVolumes, avgUsed, nil
}

// overrideVolumeIO replaces the IOPS and ThroughputBytesPerSec fields in volumesByNode
// with values from Prometheus. Volumes not present in the Prometheus result are left
// unchanged (REST API fallback).
func overrideVolumeIO(
	volumesByNode map[string][]webapi.VolumeInfo,
	io map[string]promlatency.VolumeIOMetrics,
) {
	for nodeUUID, vols := range volumesByNode {
		for i, v := range vols {
			if m, ok := io[v.UUID]; ok {
				vols[i].IOPS = m.IOPS
				vols[i].ThroughputBytesPerSec = m.ThroughputBytesPerSec
			}
		}
		volumesByNode[nodeUUID] = vols
	}
}

// buildPinnedSet returns the set of volume UUIDs whose PVC carries the pinned annotation.
// It looks up PVCs across all namespaces using the CSI volume handle pattern
// "clusterUUID:poolName:volumeUUID".
func (r *VolumeRebalancerReconciler) buildPinnedSet(
	ctx context.Context,
	clusterUUID string,
) (map[string]bool, error) {
	pinned := make(map[string]bool)

	var pvList corev1.PersistentVolumeList
	if err := r.List(ctx, &pvList); err != nil {
		return nil, fmt.Errorf("list persistent volumes: %w", err)
	}

	// Build handle-to-PVC-annotations map.
	type pvcMeta struct {
		annotations map[string]string
	}
	handleMeta := make(map[string]pvcMeta) // volumeUUID → pvcMeta

	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.CSI == nil {
			continue
		}
		parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
		if len(parts) != 3 {
			continue
		}
		pvClusterUUID, _, lvolID := parts[0], parts[1], parts[2]
		if clusterUUID != "" && pvClusterUUID != "" && pvClusterUUID != clusterUUID {
			continue
		}
		if lvolID == "" {
			continue
		}

		// Fetch the bound PVC to read its annotations.
		if pv.Spec.ClaimRef == nil {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      pv.Spec.ClaimRef.Name,
			Namespace: pv.Spec.ClaimRef.Namespace,
		}, pvc); err != nil {
			continue
		}
		handleMeta[lvolID] = pvcMeta{annotations: pvc.Annotations}
	}

	for lvolID, meta := range handleMeta {
		if v := meta.annotations[pinnedVolumeAnnotation]; v != "" {
			pinned[lvolID] = true
		}
	}
	return pinned, nil
}

// filterEligibleVolumes removes volumes that are pinned, in cool-down, not online,
// or already undergoing a migration (Migrating == true). The Migrating check covers
// the operator-restart case where the in-memory cool-down map has been reset but the
// backend still reports an active migration for the volume.
func (r *VolumeRebalancerReconciler) filterEligibleVolumes(
	vols []webapi.VolumeInfo,
	clusterUUID string,
	pinned map[string]bool,
) []webapi.VolumeInfo {
	now := time.Now()

	out := vols[:0:0]
	for _, v := range vols {
		if pinned[v.UUID] {
			continue
		}
		if r.migrationState.IsVolumeCooledDown(clusterUUID, v.UUID, now) {
			continue
		}
		if v.Status != "online" {
			continue
		}
		if v.Migrating {
			continue
		}
		out = append(out, v)
	}
	return out
}

// selectLatencyTarget returns the UUID of the healthiest target node: online, healthy,
// not the source, and with the lowest latency deviation. Nodes not yet measured (deviation = 0)
// are ranked equally lowest, which makes them valid migration targets.
func (r *VolumeRebalancerReconciler) selectLatencyTarget(
	nodeMap map[string]webapi.StorageNodeInfo,
	deviations map[string]float64,
	sourceNodeUUID string,
) string {
	type candidate struct {
		uuid      string
		deviation float64
	}
	var eligible []candidate
	for uuid, info := range nodeMap {
		if uuid == sourceNodeUUID {
			continue
		}
		if info.Status != "online" || !info.Healthy {
			continue
		}
		eligible = append(eligible, candidate{uuid, deviations[uuid]})
	}
	if len(eligible) == 0 {
		return ""
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].deviation < eligible[j].deviation })
	return eligible[0].uuid
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

// patchRebalancingMetrics updates status.rebalancingMetrics with the current deviation state.
// volumesByNode may be nil when called before volume collection (early-exit paths); in that
// case VolumeCount is left as 0.
func (r *VolumeRebalancerReconciler) patchRebalancingMetrics(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	deviations map[string]float64,
	volumesByNode map[string][]webapi.VolumeInfo,
	maxDev, avgDev float64,
	hottestNode, coolestNode string,
	now time.Time,
) {
	log := logf.FromContext(ctx)
	orig := clusterCR.DeepCopy()

	nodeMetricsList := make([]simplyblockv1alpha1.NodeLoadMetrics, 0, len(deviations))
	for uuid, dev := range deviations {
		nodeMetricsList = append(nodeMetricsList, simplyblockv1alpha1.NodeLoadMetrics{
			NodeUUID:            uuid,
			LatencyDeviationPct: dev,
			VolumeCount:         len(volumesByNode[uuid]),
			LastUpdated:         metav1.NewTime(now),
		})
	}

	nowMeta := metav1.NewTime(now)
	clusterCR.Status.RebalancingMetrics = &simplyblockv1alpha1.RebalancingMetrics{
		AvgDeviationPct:  avgDev,
		MaxDeviationPct:  maxDev,
		HottestNodeUUID:  hottestNode,
		CoolestNodeUUID:  coolestNode,
		ImbalancePercent: maxDev,
		LastEvaluatedAt:  &nowMeta,
		NodeMetrics:      nodeMetricsList,
	}

	if err := r.Status().Patch(ctx, clusterCR, client.MergeFrom(orig)); err != nil {
		log.Error(err, "Failed to patch rebalancingMetrics status")
	}
}

// collectLatencyState builds a nodeUUID → nodeLatencyData map by combining:
//   - current p99 write latency queried from Prometheus (written by the fio-bench-probe sidecar)
//   - baseline p99 write latency read from StorageNode CR status (set once by the baseline Job)
func (r *VolumeRebalancerReconciler) collectLatencyState(
	ctx context.Context,
	namespace, clusterUUID, PrometheusURL string,
) (map[string]nodeLatencyData, error) {
	provider, err := promlatency.New(PrometheusURL)
	if err != nil {
		return nil, fmt.Errorf("create prometheus latency provider: %w", err)
	}
	currentByNode, err := provider.GetClusterCurrentP99(ctx, clusterUUID)
	if err != nil {
		return nil, fmt.Errorf("query current latency from prometheus: %w", err)
	}

	baselineByNode := r.readBaselineFromCRs(ctx, namespace)

	result := make(map[string]nodeLatencyData, len(currentByNode))
	for nodeUUID, curr := range currentByNode {
		result[nodeUUID] = nodeLatencyData{
			baselineP99NS: baselineByNode[nodeUUID], // 0 until baseline Job completes
			currentP99NS:  curr,
		}
	}
	return result, nil
}

// readBaselineFromCRs returns a nodeUUID → BaselineP99NS map from all StorageNode CRs
// in the given namespace. The baseline is set exactly once by the one-shot baseline Job.
func (r *VolumeRebalancerReconciler) readBaselineFromCRs(
	ctx context.Context,
	namespace string,
) map[string]int64 {
	result := make(map[string]int64)
	var snodeList simplyblockv1alpha1.StorageNodeList
	if err := r.List(ctx, &snodeList, client.InNamespace(namespace)); err != nil {
		return result
	}
	for _, snode := range snodeList.Items {
		for _, lm := range snode.Status.LatencyMetrics {
			if lm.BaselineP99NS > 0 {
				result[lm.NodeUUID] = lm.BaselineP99NS
			}
		}
	}
	return result
}

// deviationStats computes aggregate statistics from a nodeUUID → deviationPct map.
// Returns maxDeviation, avgDeviation, hottestNodeUUID (highest deviation),
// and coolestNodeUUID (lowest deviation). All values are 0 / empty when the map is empty.
func deviationStats(deviations map[string]float64) (
	maxDev, avgDev float64,
	hottest, coolest string,
) {
	if len(deviations) == 0 {
		return
	}
	var sum float64
	first := true
	for uuid, dev := range deviations {
		sum += dev
		if first || dev > maxDev {
			maxDev = dev
			hottest = uuid
		}
		if first || dev < avgDev {
			avgDev = dev
			coolest = uuid
		}
		first = false
	}
	avgDev = sum / float64(len(deviations))
	return
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
