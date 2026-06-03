/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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
	defaultEvaluationInterval          = 60 * time.Second
	defaultImbalanceThresholdPct       = 20
	defaultCoolDownSeconds             = 60
	defaultMaxVolumeMigrationsPerCycle = 10

	// migrationInitialDelay is the minimum time after calling CreateMigration before
	// Phase 6b begins polling the migration status. Prevents a race between the API
	// call and the control-plane migration tracker.
	migrationInitialDelay = 20 * time.Second
	// migrationStuckWarningTimeout triggers a Warning event when a migration has
	// not switched over within this duration.
	migrationStuckWarningTimeout = 30 * time.Minute

	// migrationBudgetFraction is the fraction of the source node's total volume IO score
	// that may be migrated in a single evaluation cycle.
	migrationBudgetFraction = 0.10

	// defaultIOPSWeight is the default weight applied to per-volume IOPS in volumeIOScore.
	defaultIOPSWeight = 1.0
	// defaultThroughputMBWeight is the default weight applied to per-volume throughput (MB/s).
	defaultThroughputMBWeight = 0.1
)

// pendingMigrationState tracks a volume through the async migration lifecycle.
type pendingMigrationState string

const (
	// pendingStateWaitingForCompletion is set immediately after CreateMigration.
	// The reconciler polls GetMigration until CompletedAt > 0.
	pendingStateWaitingForCompletion pendingMigrationState = "waiting_for_completion"
)

type pendingMigration struct {
	state          pendingMigrationState
	migrationStart time.Time
	migrationID    string // ID returned by CreateMigration
	clusterUUID    string
	poolUUID       string
	stuckWarned    bool
}

// nodeLatencyData holds the fio p99 latency measurements for one storage node.
type nodeLatencyData struct {
	baselineP99NS int64
	currentP99NS  int64
}

// rebalancingConfig holds resolved (defaults applied) values from VolumeRebalancingSpec.
type rebalancingConfig struct {
	evalInterval       time.Duration
	prometheusURL      string
	imbalanceThreshold float64
	iopsWeight         float64
	throughputWeight   float64
	maxMigrations      int
	coolDownSecs       int64
}

// resolveRebalancingConfig applies defaults and validates the spec. It returns an error
// when prometheusURL is missing, which is the only hard requirement.
func resolveRebalancingConfig(spec *simplyblockv1alpha1.VolumeRebalancingSpec) (rebalancingConfig, error) {
	cfg := rebalancingConfig{
		evalInterval:       defaultEvaluationInterval,
		imbalanceThreshold: defaultImbalanceThresholdPct,
		iopsWeight:         defaultIOPSWeight,
		throughputWeight:   defaultThroughputMBWeight,
		maxMigrations:      defaultMaxVolumeMigrationsPerCycle,
		coolDownSecs:       defaultCoolDownSeconds,
	}
	if spec.EvaluationInterval != nil && spec.EvaluationInterval.Duration > 0 {
		cfg.evalInterval = spec.EvaluationInterval.Duration
	}
	if spec.PrometheusURL == nil || *spec.PrometheusURL == "" {
		return cfg, fmt.Errorf("spec.volumeRebalancing.prometheusURL is required")
	}
	cfg.prometheusURL = *spec.PrometheusURL
	if spec.ImbalanceThreshold != nil {
		cfg.imbalanceThreshold = float64(*spec.ImbalanceThreshold)
	}
	if spec.IOPSWeight != nil && *spec.IOPSWeight > 0 {
		cfg.iopsWeight = *spec.IOPSWeight
	}
	if spec.ThroughputWeight != nil && *spec.ThroughputWeight > 0 {
		cfg.throughputWeight = *spec.ThroughputWeight
	}
	if spec.MaxVolumeMigrationsPerCycle != nil && *spec.MaxVolumeMigrationsPerCycle > 0 {
		cfg.maxMigrations = int(*spec.MaxVolumeMigrationsPerCycle)
	}
	if spec.DefaultCoolDownSeconds != nil {
		cfg.coolDownSecs = int64(*spec.DefaultCoolDownSeconds)
	}
	return cfg, nil
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

	mu sync.Mutex
	// coolDownMap keys: "clusterUUID/volumeUUID" → expiry time
	coolDownMap map[string]time.Time
	// pendingMigrations keys: "clusterUUID/volumeUUID"
	pendingMigrations map[string]*pendingMigration
}

func (r *VolumeRebalancerReconciler) init() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.coolDownMap == nil {
		r.coolDownMap = make(map[string]time.Time)
		r.pendingMigrations = make(map[string]*pendingMigration)
	}
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch

func (r *VolumeRebalancerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	cfg, err := resolveRebalancingConfig(spec)
	if err != nil {
		log.Error(err, "Invalid rebalancing configuration; skipping cycle")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		return ctrl.Result{RequeueAfter: defaultEvaluationInterval}, nil
	}
	cycleStart := time.Now()

	clusterUUID, clusterSecret, err := utils.GetClusterAuth(ctx, r.Client, clusterCR.Namespace, clusterCR.Name)
	if err != nil {
		log.Error(err, "Cannot get cluster auth; requeuing")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.processPendingMigrations(ctx, clusterCR, clusterUUID, clusterSecret)

	now := time.Now()

	nodes, err := r.apiClient.GetStorageNodes(ctx, clusterSecret, clusterUUID)
	if err != nil {
		log.Error(err, "Cannot list storage nodes; requeuing")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	nodeMap := make(map[string]webapi.StorageNodeInfo, len(nodes))
	for _, n := range nodes {
		nodeMap[n.UUID] = n
	}

	deviations, maxDev, avgDev, hottestNode, coolestNode, err := r.computeLatencyDeviations(ctx, clusterCR, clusterUUID, cfg.prometheusURL)
	if err != nil {
		log.Error(err, "Cannot collect latency from Prometheus; requeuing")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if hasOfflineNode(nodeMap) {
		log.Info("Cluster has offline node(s); skipping rebalancing cycle")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.evalInterval)}, nil
	}

	hotNodes := nodesAboveThreshold(deviations, cfg.imbalanceThreshold)
	if len(hotNodes) == 0 {
		log.V(1).Info("No node exceeds latency deviation threshold; skipping",
			"maxDeviationPct", maxDev, "threshold", cfg.imbalanceThreshold)
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		r.patchRebalancingMetrics(ctx, clusterCR, deviations, nil, maxDev, avgDev, hottestNode, coolestNode, now)
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.evalInterval)}, nil
	}

	volumesByNode, allVolumes, err := r.collectAndEnrichVolumes(ctx, clusterSecret, clusterUUID, cfg.prometheusURL)
	if err != nil {
		log.Error(err, "Cannot collect volume placement; requeuing")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	pinnedVolumeUUIDs, err := r.buildPinnedSet(ctx, clusterUUID)
	if err != nil {
		log.Error(err, "Cannot build pinned volume set; requeuing")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	sourceNodeUUID, candidates := r.selectSourceNode(hotNodes, volumesByNode, allVolumes, clusterUUID, pinnedVolumeUUIDs, cfg.iopsWeight, cfg.throughputWeight)
	if sourceNodeUUID == "" {
		log.Info("All latency-hot nodes have no eligible migration candidates (pinned or in cool-down)")
		r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingBlocked",
			"All %d latency-degraded node(s) have no eligible migration candidates. Pinned volumes or active cool-downs are preventing rebalancing.",
			len(hotNodes))
		rebalancerPinnedBlockedTotal.WithLabelValues(clusterCR.Name).Inc()
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "blocked").Inc()
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.evalInterval)}, nil
	}

	toMigrate := selectMigrationSet(candidates, cfg.maxMigrations)
	if len(toMigrate) == 0 {
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.evalInterval)}, nil
	}

	r.mu.Lock()
	hasPending := r.hasPendingMigrationsForCluster(clusterUUID)
	r.mu.Unlock()
	if hasPending {
		log.V(1).Info("Pending migrations exist; deferring new migrations to next cycle")
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
		r.patchRebalancingMetrics(ctx, clusterCR, deviations, volumesByNode, maxDev, avgDev, hottestNode, coolestNode, now)
		return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.evalInterval)}, nil
	}

	if err := r.setRebalancing(ctx, clusterCR, true); err != nil {
		log.Error(err, "Failed to set status.rebalancing=true")
	}
	defer func() {
		if err := r.setRebalancing(ctx, clusterCR, false); err != nil {
			log.Error(err, "Failed to clear status.rebalancing")
		}
	}()

	migratedCount := r.executeMigrations(ctx, clusterCR, toMigrate,
		sourceNodeUUID, clusterUUID, clusterSecret, nodeMap, deviations, cfg.coolDownSecs,
		cycleStart.Add(cfg.evalInterval))

	r.patchRebalancingMetrics(ctx, clusterCR, deviations, volumesByNode, maxDev, avgDev, hottestNode, coolestNode, now)

	r.mu.Lock()
	activeCooldowns := r.countClusterCooldowns(clusterUUID, now)
	r.mu.Unlock()
	rebalancerCooldownVolumes.WithLabelValues(clusterCR.Name).Set(float64(activeCooldowns))

	if migratedCount > 0 {
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "migrated").Inc()
	} else {
		rebalancerEvaluationTotal.WithLabelValues(clusterCR.Name, "skipped").Inc()
	}

	return ctrl.Result{RequeueAfter: requeueAfter(cycleStart, cfg.evalInterval)}, nil
}

// computeLatencyDeviations collects per-node latency from Prometheus and StorageNode CRs,
// computes deviation from baseline, emits Prometheus gauges, and returns the deviation map
// plus cluster-level statistics.
func (r *VolumeRebalancerReconciler) computeLatencyDeviations(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	clusterUUID, prometheusURL string,
) (deviations map[string]float64, maxDev, avgDev float64, hottestNode, coolestNode string, err error) {
	latencyByNode, err := r.collectLatencyState(ctx, clusterCR.Namespace, clusterUUID, prometheusURL)
	if err != nil {
		return nil, 0, 0, "", "", err
	}
	deviations = make(map[string]float64, len(latencyByNode))
	for nodeUUID, ld := range latencyByNode {
		deviations[nodeUUID] = computeLatencyDeviationPct(ld.baselineP99NS, ld.currentP99NS)
	}
	for nodeUUID, dev := range deviations {
		rebalancerNodeLatencyDeviationPct.WithLabelValues(clusterCR.Name, nodeUUID).Set(dev)
	}
	maxDev, avgDev, hottestNode, coolestNode = deviationStats(deviations)
	rebalancerMaxLatencyDeviationPct.WithLabelValues(clusterCR.Name).Set(maxDev)
	return deviations, maxDev, avgDev, hottestNode, coolestNode, nil
}

// collectAndEnrichVolumes lists all volumes per node and overlays IOPS/throughput from
// Prometheus. On Prometheus failure it falls back to REST API values (may be zero).
func (r *VolumeRebalancerReconciler) collectAndEnrichVolumes(
	ctx context.Context,
	clusterSecret, clusterUUID, prometheusURL string,
) (volumesByNode map[string][]webapi.VolumeInfo, allVolumes map[string]volumePlacement, err error) {
	log := logf.FromContext(ctx)
	volumesByNode, allVolumes, _, err = r.collectVolumesByNode(ctx, clusterSecret, clusterUUID)
	if err != nil {
		return nil, nil, err
	}
	if ioProvider, pErr := promlatency.New(prometheusURL); pErr != nil {
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
	iopsWeight, throughputWeight float64,
) (sourceNodeUUID string, candidates []rankedCandidate) {
	for _, nodeUUID := range hotNodes {
		eligible := r.filterEligibleVolumes(volumesByNode[nodeUUID], clusterUUID, pinnedVolumeUUIDs)
		if len(eligible) == 0 {
			continue
		}
		ranked := make([]rankedCandidate, 0, len(eligible))
		for _, vol := range eligible {
			cs := volumeIOScore(vol.IOPS, vol.ThroughputBytesPerSec, iopsWeight, throughputWeight)
			ranked = append(ranked, rankedCandidate{vol: vol, score: cs, pool: allVolumes[vol.UUID].poolUUID})
		}
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
		return nodeUUID, ranked
	}
	return "", nil
}

// selectMigrationSet selects at most maxMigrations candidates from the ranked list,
// capped by a 10% IO-budget fraction of the total candidate score.
func selectMigrationSet(candidates []rankedCandidate, maxMigrations int) []rankedCandidate {
	var totalScore float64
	for _, rc := range candidates {
		totalScore += rc.score
	}
	budget := migrationBudgetFraction * totalScore
	toMigrate := make([]rankedCandidate, 0, maxMigrations)
	for _, rc := range candidates {
		if len(toMigrate) == 0 || rc.score <= budget {
			toMigrate = append(toMigrate, rc)
			budget -= rc.score
		}
		if len(toMigrate) >= maxMigrations {
			break
		}
	}
	return toMigrate
}

// executeMigrations calls CreateMigration for each candidate, tracks cool-down and
// pending state, and returns the number of migrations successfully initiated.
func (r *VolumeRebalancerReconciler) executeMigrations(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	toMigrate []rankedCandidate,
	sourceNodeUUID, clusterUUID, clusterSecret string,
	nodeMap map[string]webapi.StorageNodeInfo,
	deviations map[string]float64,
	coolDownSecs int64,
	cycleDeadline time.Time,
) int {
	log := logf.FromContext(ctx)
	migratedCount := 0
	for _, rc := range toMigrate {
		if time.Now().After(cycleDeadline) {
			r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingDeferred",
				"Cycle deadline reached; %d migration candidate(s) deferred to next cycle",
				len(toMigrate)-migratedCount)
			break
		}
		targetUUID := r.selectLatencyTarget(nodeMap, deviations, sourceNodeUUID)
		if targetUUID == "" {
			log.Info("No suitable target node for volume; skipping", "volume", rc.vol.UUID)
			continue
		}
		migration, err := r.apiClient.CreateMigration(ctx, clusterSecret, clusterUUID, rc.vol.UUID, targetUUID)
		if err != nil {
			log.Error(err, "CreateMigration failed", "volume", rc.vol.UUID, "target", targetUUID)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingFailed",
				"Migration of volume %s to node %s failed: %v", rc.vol.UUID, targetUUID, err)
			continue
		}
		cdKey := clusterUUID + "/" + rc.vol.UUID
		r.mu.Lock()
		r.coolDownMap[cdKey] = time.Now().Add(time.Duration(coolDownSecs) * time.Second)
		r.pendingMigrations[cdKey] = &pendingMigration{
			state:          pendingStateWaitingForCompletion,
			migrationStart: time.Now(),
			migrationID:    migration.ID,
			clusterUUID:    clusterUUID,
			poolUUID:       rc.pool,
		}
		r.mu.Unlock()
		r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingStarted",
			"Initiating migration of volume %s from node %s to %s (latency deviation: %.1f%%)",
			rc.vol.UUID, sourceNodeUUID, targetUUID, deviations[sourceNodeUUID])
		rebalancerMigrationsTotal.WithLabelValues(clusterCR.Name, sourceNodeUUID, targetUUID).Inc()
		migratedCount++
	}
	return migratedCount
}

// processPendingMigrations polls the migration API for all in-progress migrations
// belonging to the given cluster and removes entries once they complete.
func (r *VolumeRebalancerReconciler) processPendingMigrations(
	ctx context.Context,
	clusterCR *simplyblockv1alpha1.StorageCluster,
	clusterUUID, clusterSecret string,
) {
	log := logf.FromContext(ctx)
	prefix := clusterUUID + "/"
	now := time.Now()

	r.mu.Lock()
	keys := make([]string, 0)
	for k := range r.pendingMigrations {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	r.mu.Unlock()

	for _, key := range keys {
		r.mu.Lock()
		pm, ok := r.pendingMigrations[key]
		if !ok {
			r.mu.Unlock()
			continue
		}
		migrationID := pm.migrationID
		migStart := pm.migrationStart
		stuckWarned := pm.stuckWarned
		r.mu.Unlock()

		volumeUUID := strings.TrimPrefix(key, prefix)

		// Enforce the initial delay to avoid a race between CreateMigration and
		// the control-plane tracker populating the migration record.
		if now.Before(migStart.Add(migrationInitialDelay)) {
			continue
		}

		migration, err := r.apiClient.GetMigration(ctx, clusterSecret, clusterUUID, migrationID)
		if err != nil {
			log.Error(err, "Cannot get migration status", "migration", migrationID, "volume", volumeUUID)
			continue
		}

		if migration.CompletedAt > 0 {
			r.mu.Lock()
			delete(r.pendingMigrations, key)
			r.mu.Unlock()
			if migration.ErrorMessage != "" {
				log.Error(nil, "Volume migration completed with error",
					"volume", volumeUUID, "migration", migrationID, "error", migration.ErrorMessage)
				r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingFailed",
					"Migration %s of volume %s completed with error: %s",
					migrationID, volumeUUID, migration.ErrorMessage)
			} else {
				log.Info("Volume migration complete", "volume", volumeUUID, "migration", migrationID)
				r.Recorder.Eventf(clusterCR, corev1.EventTypeNormal, "VolumeRebalancingComplete",
					"Migration %s of volume %s completed successfully", migrationID, volumeUUID)
			}
			continue
		}

		if !stuckWarned && now.After(migStart.Add(migrationStuckWarningTimeout)) {
			log.Error(nil, "Volume migration has not completed within 30 minutes",
				"volume", volumeUUID, "migration", migrationID)
			r.Recorder.Eventf(clusterCR, corev1.EventTypeWarning, "VolumeRebalancingStuck",
				"Migration %s of volume %s has not completed after 30 minutes (phase: %s, status: %s)",
				migrationID, volumeUUID, migration.Phase, migration.Status)
			r.mu.Lock()
			if m, exists := r.pendingMigrations[key]; exists {
				m.stuckWarned = true
			}
			r.mu.Unlock()
		}
	}
}

// collectVolumesByNode lists all volumes across all pools and returns:
// - volumesByNode: nodeUUID → []VolumeInfo
// - allVolumes: volumeUUID → {VolumeInfo, poolUUID}
// - clusterAvgUsedBytes: mean used bytes across all volumes
func (r *VolumeRebalancerReconciler) collectVolumesByNode(
	ctx context.Context,
	clusterSecret, clusterUUID string,
) (map[string][]webapi.VolumeInfo, map[string]volumePlacement, int64, error) {
	pools, err := r.apiClient.GetStoragePools(ctx, clusterSecret, clusterUUID)
	if err != nil {
		return nil, nil, 0, err
	}

	volumesByNode := make(map[string][]webapi.VolumeInfo)
	allVolumes := make(map[string]volumePlacement)

	var totalUsed int64
	var volumeCount int64

	for _, pool := range pools {
		vols, err := r.apiClient.GetPoolVolumes(ctx, clusterSecret, clusterUUID, pool.UUID)
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
func overrideVolumeIO(volumesByNode map[string][]webapi.VolumeInfo, io map[string]promlatency.VolumeIOMetrics) {
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
func (r *VolumeRebalancerReconciler) buildPinnedSet(ctx context.Context, clusterUUID string) (map[string]bool, error) {
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
	r.mu.Lock()
	defer r.mu.Unlock()

	out := vols[:0:0]
	for _, v := range vols {
		if pinned[v.UUID] {
			continue
		}
		if expiry, inCD := r.coolDownMap[clusterUUID+"/"+v.UUID]; inCD && now.Before(expiry) {
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

// hasPendingMigrationsForCluster reports whether any migration is still tracked in the
// 6b/6c state machine for the given cluster. Must be called with r.mu held.
func (r *VolumeRebalancerReconciler) hasPendingMigrationsForCluster(clusterUUID string) bool {
	prefix := clusterUUID + "/"
	for k := range r.pendingMigrations {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// countClusterCooldowns returns the number of non-expired cool-down entries for the cluster.
func (r *VolumeRebalancerReconciler) countClusterCooldowns(clusterUUID string, now time.Time) int {
	prefix := clusterUUID + "/"
	count := 0
	for k, expiry := range r.coolDownMap {
		if strings.HasPrefix(k, prefix) && now.Before(expiry) {
			count++
		}
	}
	return count
}

// collectLatencyState builds a nodeUUID → nodeLatencyData map by combining:
//   - current p99 write latency queried from Prometheus (written by the fio-bench-probe sidecar)
//   - baseline p99 write latency read from StorageNode CR status (set once by the baseline Job)
func (r *VolumeRebalancerReconciler) collectLatencyState(
	ctx context.Context,
	namespace, clusterUUID, prometheusURL string,
) (map[string]nodeLatencyData, error) {
	provider, err := promlatency.New(prometheusURL)
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
func (r *VolumeRebalancerReconciler) readBaselineFromCRs(ctx context.Context, namespace string) map[string]int64 {
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
func deviationStats(deviations map[string]float64) (maxDev, avgDev float64, hottest, coolest string) {
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
func hasOfflineNode(nodeMap map[string]webapi.StorageNodeInfo) bool {
	for _, n := range nodeMap {
		switch n.Status {
		case "offline", "in_restart", "unreachable":
			return true
		}
	}
	return false
}

// requeueAfter returns the time remaining until the next evaluation, clamped to 0.
func requeueAfter(cycleStart time.Time, evalInterval time.Duration) time.Duration {
	remaining := evalInterval - time.Since(cycleStart)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (r *VolumeRebalancerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.apiClient = webapi.NewClient()
	return ctrl.NewControllerManagedBy(mgr).
		For(&simplyblockv1alpha1.StorageCluster{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("volumerebalancer").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
