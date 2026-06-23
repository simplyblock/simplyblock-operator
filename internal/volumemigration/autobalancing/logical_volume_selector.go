package autobalancing

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	promlatency "github.com/simplyblock/simplyblock-operator/internal/metrics/prometheus"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// VolumePlacement associates a VolumeInfo with the pool it belongs to.
// It is the primary unit passed between collection, filtering, and scoring stages.
type VolumePlacement struct {
	webapi.VolumeInfo
	// PoolUUID is the storage pool that contains this volume.
	PoolUUID string
}

// RankedCandidate pairs an eligible VolumePlacement with its computed IO score,
// used to order migration candidates within a source node (highest score first).
type RankedCandidate struct {
	// Vol is the volume and its pool association.
	Vol VolumePlacement
	// Score is the combined IOPS + throughput priority score (see VolumeIOScore).
	Score float64
}

// LogicalVolumeSelectorInput provides the per-cluster context needed to collect
// and filter volumes for migration candidacy.
type LogicalVolumeSelectorInput struct {
	// ClusterUUID is the storage cluster to query.
	ClusterUUID string
	// PrometheusURL is the Prometheus endpoint used to enrich volumes with live
	// IOPS and throughput data. On error the REST API values are kept as-is.
	PrometheusURL string
	// IsCoolingDown returns true when the given volume UUID is still within its
	// post-migration cool-down window and must not be migrated again yet.
	IsCoolingDown func(volumeUUID string) bool
	// Pinned is the set of volume UUIDs excluded from migration regardless of
	// load (set via the simplyblock.io/pinned-volume PVC annotation).
	Pinned map[string]bool
}

// LogicalVolumeSelector collects volumes from the storage API, enriches them
// with live I/O metrics from Prometheus, filters them for migration eligibility,
// and selects the ranked set to migrate in a single evaluation cycle.
// It is the logical-volume counterpart of StorageNodeSelector.
type LogicalVolumeSelector struct {
	apiClient *webapi.Client
	// k8sClient is used by BuildPinnedSet to read PVC annotations.
	k8sClient client.Client
}

// NewLogicalVolumeSelector creates a LogicalVolumeSelector backed by the given
// storage API client and Kubernetes client.
func NewLogicalVolumeSelector(
	apiClient *webapi.Client,
	k8sClient client.Client,
) *LogicalVolumeSelector {
	return &LogicalVolumeSelector{apiClient: apiClient, k8sClient: k8sClient}
}

// SelectVolumesForMigration is the main selection entry point. Given the list of
// hot nodes sorted worst-first (from StorageNodeSelector.SelectStorageNodes), it:
//
//  1. Iterates hot nodes and picks the first that has at least one eligible volume.
//  2. Ranks eligible volumes on that node by VolumeIOScore descending.
//  3. Applies the 10 % IO-budget cap and cfg.MaxMigrations hard limit to produce
//     the final migration set (§6 Steps 2–4 of the design document).
//
// Returns the source node UUID and the ranked candidates to migrate. Both are
// empty/nil when every hot node has no eligible volumes.
func (lvs *LogicalVolumeSelector) SelectVolumesForMigration(
	input LogicalVolumeSelectorInput,
	hotNodes []string,
	volumesByNode map[string][]VolumePlacement,
	cfg RebalancingConfig,
) (sourceNodeUUID string, toMigrate []RankedCandidate) {
	for _, nodeUUID := range hotNodes {
		eligible := lvs.FilterEligibleVolumes(input, volumesByNode[nodeUUID])
		if len(eligible) == 0 {
			continue
		}

		// Rank by IO score descending — highest load migrated first.
		ranked := make([]RankedCandidate, 0, len(eligible))
		for _, vp := range eligible {
			score := volumemigration.VolumeIOScore(vp.IOPS, vp.ThroughputBytesPerSec, cfg.IopsWeight, cfg.ThroughputWeight)
			ranked = append(ranked, RankedCandidate{Vol: vp, Score: score})
		}
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })

		return nodeUUID, lvs.selectMigrationSet(ranked, cfg.MaxMigrations)
	}
	return "", nil
}

// CollectVolumes fetches all volumes across every pool in the cluster, enriches
// each with live IOPS and throughput from Prometheus (falling back to REST API
// values when Prometheus is unavailable), and returns:
//   - volumesByNode: nodeUUID → []VolumePlacement, including ineligible volumes
//   - allVolumes:    volumeUUID → VolumePlacement, for O(1) lookup by ID
func (lvs *LogicalVolumeSelector) CollectVolumes(
	ctx context.Context,
	input LogicalVolumeSelectorInput,
) (volumesByNode map[string][]VolumePlacement, allVolumes map[string]VolumePlacement, err error) {
	log := logf.FromContext(ctx)

	// The rebalancer only ever acts on PV/PVC-managed volumes. Resolve the set of
	// simplyblock CSI-managed volume UUIDs up front and restrict collection to them
	// so backend-only volumes (e.g. benchmark probes) never enter the candidate pool.
	managed, err := lvs.BuildCSIManagedVolumes(ctx, input.ClusterUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("build CSI-managed volume set: %w", err)
	}

	volumesByNode, allVolumes = lvs.collectVolumesByNode(ctx, input.ClusterUUID, managed)

	if ioProvider, pErr := promlatency.New(input.PrometheusURL); pErr != nil {
		log.Error(pErr, "Cannot create volume IO provider; scoring will use REST API values")
	} else if prometheusIO, pErr := ioProvider.GetClusterVolumeIO(ctx, input.ClusterUUID); pErr != nil {
		log.Error(pErr, "Cannot query volume IO from Prometheus; scoring will use REST API values")
	} else {
		lvs.overrideVolumeIO(volumesByNode, prometheusIO)
	}

	return volumesByNode, allVolumes, nil
}

// FilterEligibleVolumes returns the subset of vols that are candidates for
// migration. A volume is excluded when any of the following is true:
//   - it appears in input.Pinned (simplyblock.io/pinned-volume annotation)
//   - input.IsCoolingDown returns true for it (post-migration cool-down)
//   - its Status is not "online"
//   - its Migrating flag is set (guards against re-migration after operator restart
//     when the in-memory cool-down map is empty)
func (lvs *LogicalVolumeSelector) FilterEligibleVolumes(
	input LogicalVolumeSelectorInput,
	vols []VolumePlacement,
) []VolumePlacement {
	out := vols[:0:0]
	for _, vp := range vols {
		if input.Pinned[vp.UUID] {
			continue
		}
		if input.IsCoolingDown != nil && input.IsCoolingDown(vp.UUID) {
			continue
		}
		if vp.Status != "online" {
			continue
		}
		if vp.Migrating {
			continue
		}
		out = append(out, vp)
	}
	return out
}

// collectVolumesByNode builds the migration candidate set by fetching exactly the
// CSI-managed volumes (one backend GET each, using the cluster/pool/volume UUIDs from
// the PV handle) — no pool enumeration. A volume that errors or no longer exists in
// the backend is skipped rather than aborting the cycle. Returns:
//   - volumesByNode: nodeUUID → []VolumePlacement (CSI-managed volumes only)
//   - allVolumes:    volumeUUID → VolumePlacement, for O(1) lookup
func (lvs *LogicalVolumeSelector) collectVolumesByNode(
	ctx context.Context,
	clusterUUID string,
	managed []managedVolume,
) (volumesByNode map[string][]VolumePlacement, allVolumes map[string]VolumePlacement) {
	log := logf.FromContext(ctx)

	volumesByNode = make(map[string][]VolumePlacement)
	allVolumes = make(map[string]VolumePlacement)

	for _, mv := range managed {
		v, err := lvs.apiClient.GetVolume(ctx, clusterUUID, mv.PoolUUID, mv.VolumeUUID)
		if err != nil {
			// One unreachable/erroring volume must not abort the whole cycle.
			log.Error(err, "skipping volume: cannot fetch from backend", "volume", mv.VolumeUUID)
			continue
		}
		if v == nil {
			log.V(1).Info("CSI-managed volume absent from backend; skipping", "volume", mv.VolumeUUID)
			continue
		}
		vp := VolumePlacement{VolumeInfo: *v, PoolUUID: mv.PoolUUID}
		volumesByNode[v.PrimaryNodeUUID] = append(volumesByNode[v.PrimaryNodeUUID], vp)
		allVolumes[v.UUID] = vp
	}
	return volumesByNode, allVolumes
}

// selectMigrationSet applies a greedy 10 % IO-budget cap and a hard MaxMigrations
// limit to the ranked candidate list. At least one volume is always included when
// the list is non-empty (so a degraded node is never left without a migration even
// if its single candidate exceeds the budget fraction).
func (lvs *LogicalVolumeSelector) selectMigrationSet(ranked []RankedCandidate, maxMigrations int) []RankedCandidate {
	var totalScore float64
	for _, rc := range ranked {
		totalScore += rc.Score
	}
	budget := migrationBudgetFraction * totalScore
	out := make([]RankedCandidate, 0, maxMigrations)
	for _, rc := range ranked {
		if len(out) == 0 || rc.Score <= budget {
			out = append(out, rc)
			budget -= rc.Score
		}
		if len(out) >= maxMigrations {
			break
		}
	}
	return out
}

// BuildPinnedSet returns the set of volume UUIDs whose bound PVC carries the
// simplyblock.io/pinned-volume annotation. It scans all PersistentVolumes and
// resolves the volume UUID from the CSI volume handle
// ("<clusterUUID>:<poolName>:<volumeUUID>"). Pass an empty clusterUUID to include
// volumes from all clusters.
func (lvs *LogicalVolumeSelector) BuildPinnedSet(ctx context.Context, clusterUUID string) (map[string]bool, error) {
	pinned := make(map[string]bool)

	var pvList corev1.PersistentVolumeList
	if err := lvs.k8sClient.List(ctx, &pvList); err != nil {
		return nil, fmt.Errorf("list PersistentVolumes: %w", err)
	}

	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.ClaimRef == nil {
			continue
		}
		// Handle format: "<clusterUUID>:<poolName>:<volumeUUID>"
		parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		pvClusterUUID, lvolID := parts[0], parts[2]
		if clusterUUID != "" && pvClusterUUID != "" && pvClusterUUID != clusterUUID {
			continue
		}

		pvc := &corev1.PersistentVolumeClaim{}
		if err := lvs.k8sClient.Get(ctx, client.ObjectKey{
			Name:      pv.Spec.ClaimRef.Name,
			Namespace: pv.Spec.ClaimRef.Namespace,
		}, pvc); err != nil {
			continue
		}
		if pvc.Annotations[pinnedVolumeAnnotation] != "" {
			pinned[lvolID] = true
		}
	}
	return pinned, nil
}

// PVCSIDriverIndexField is the field-indexer key used to filter PersistentVolumes by
// their CSI driver through the controller cache (the API server does not support a
// spec.csi.driver field selector for PVs natively). It must be registered on the
// manager's field indexer — see VolumeRebalancerReconciler.SetupWithManager.
const PVCSIDriverIndexField = "spec.csi.driver"

// managedVolume identifies a simplyblock CSI-managed volume fully from its PV's CSI
// volume handle ("<clusterUUID>:<poolUUID>:<volumeUUID>") — enough to fetch exactly
// that volume from the backend without enumerating any pool.
type managedVolume struct {
	PoolUUID   string
	VolumeUUID string
}

// BuildCSIManagedVolumes returns the simplyblock CSI-managed volumes in the given
// cluster, derived from the PersistentVolumes. The rebalancer only ever acts on
// PV/PVC-managed volumes; backend-only volumes (e.g. the per-node benchmark probes
// "simplyblock-rebalancer-<nodeUUID>", which have no PV) are therefore never
// candidates. Pass an empty clusterUUID to include all clusters.
//
// The List is filtered server-side (via the cache field index) to PVs whose CSI
// driver is the simplyblock provisioner, so non-simplyblock PVs never reach here.
func (lvs *LogicalVolumeSelector) BuildCSIManagedVolumes(ctx context.Context, clusterUUID string) ([]managedVolume, error) {
	var pvList corev1.PersistentVolumeList
	if err := lvs.k8sClient.List(ctx, &pvList,
		client.MatchingFields{PVCSIDriverIndexField: utils.CSIProvisioner}); err != nil {
		return nil, fmt.Errorf("list simplyblock CSI PersistentVolumes: %w", err)
	}

	var managed []managedVolume
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.CSI == nil {
			continue
		}
		// Handle format: "<clusterUUID>:<poolUUID>:<volumeUUID>"
		parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, ":", 3)
		if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			continue
		}
		if clusterUUID != "" && parts[0] != "" && parts[0] != clusterUUID {
			continue
		}
		managed = append(managed, managedVolume{PoolUUID: parts[1], VolumeUUID: parts[2]})
	}
	return managed, nil
}

// overrideVolumeIO replaces the IOPS and ThroughputBytesPerSec fields on each
// volume in volumesByNode with live data queried from Prometheus. Volumes absent
// from the Prometheus result keep their REST API values, which may be zero when
// no I/O has been reported yet.
func (lvs *LogicalVolumeSelector) overrideVolumeIO(
	volumesByNode map[string][]VolumePlacement,
	io map[string]promlatency.VolumeIOMetrics,
) {
	for nodeUUID, vols := range volumesByNode {
		for i, vp := range vols {
			if m, ok := io[vp.UUID]; ok {
				vols[i].IOPS = m.IOPS
				vols[i].ThroughputBytesPerSec = m.ThroughputBytesPerSec
			}
		}
		volumesByNode[nodeUUID] = vols
	}
}
