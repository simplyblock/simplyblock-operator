package autobalancing

import (
	"context"
	"fmt"
)

// MigrationCandidate is the concrete output of the rebalancing algorithm: a single
// volume that should be migrated from SourceNodeUUID to TargetNodeUUID.
type MigrationCandidate struct {
	// ClusterUUID is the storage cluster that owns the volume (the source cluster).
	ClusterUUID string
	// SourceNodeUUID is the hot node the volume currently resides on.
	SourceNodeUUID string
	// TargetClusterUUID is the cluster that owns the target node. Equal to ClusterUUID
	// today; carried separately so the type is ready for cross-cluster migration.
	TargetClusterUUID string
	// TargetNodeUUID is the chosen migration destination node.
	TargetNodeUUID string
	// Volume is the volume to migrate, including its pool association and IO metrics.
	Volume VolumePlacement
}

type clusterWork struct {
	// hotNodes is the ordered list of source node UUIDs (worst deviation first).
	hotNodes []string
	// targetBySource maps each source node UUID to its assigned migration target node.
	targetBySource map[string]string
	// targetClusterBySource maps each source node UUID to its target node's cluster
	// (equal to the source cluster today; differs once cross-cluster is enabled).
	targetClusterBySource map[string]string
}

// Rebalancer wires StorageNodeSelector and LogicalVolumeSelector together to
// produce the concrete set of volume migrations for a single evaluation cycle.
// It is the top-level entry point for the auto-rebalancing algorithm.
type Rebalancer struct {
	nodeSelector   *StorageNodeSelector
	volumeSelector *LogicalVolumeSelector
}

// NewRebalancer creates a Rebalancer from pre-constructed selectors.
func NewRebalancer(nodeSelector *StorageNodeSelector, volumeSelector *LogicalVolumeSelector) *Rebalancer {
	return &Rebalancer{nodeSelector: nodeSelector, volumeSelector: volumeSelector}
}

// SelectMigrations runs the full rebalancing algorithm for one evaluation cycle:
//
//  1. StorageNodeSelector identifies hot nodes and pairs each with the cluster's
//     coolest node as the migration target.
//  2. For each affected cluster, LogicalVolumeSelector builds the pinned-volume
//     set, collects and Prometheus-enriches volumes, then selects the ranked
//     migration set (up to cfg.MaxMigrations, 10 % IO-budget cap).
//  3. Each selected volume is returned as a MigrationCandidate with its
//     source and target node already resolved.
//
// isCoolingDown is called per volume to check the post-migration cool-down
// window. The first argument is the cluster UUID, the second the volume UUID.
// Pass nil to skip the cool-down check.
// The returned pinnedBlocked is true when at least one hot node could not be
// rebalanced because every volume it hosts is pinned (simplyblock.io/pinned-volume).
// This is a distinct, legitimate outcome from "no hot nodes" or "cooling down" and
// callers surface it via the rebalancer_pinned_blocked metric.
func (rb *Rebalancer) SelectMigrations(
	ctx context.Context,
	cfg RebalancingConfig,
	isCoolingDown func(clusterUUID, volumeUUID string) bool,
	inputs ...StorageNodeSelectorInput,
) (candidates []MigrationCandidate, pinnedBlocked bool, err error) {
	// Step 1 — identify hot→cool node pairs across all clusters.
	nodePairs, err := rb.nodeSelector.SelectStorageNodes(ctx, cfg, inputs...)
	if err != nil {
		return nil, false, err
	}
	if len(nodePairs) == 0 {
		return nil, false, nil
	}

	// Step 2 — group pairs by cluster, preserving the worst-first ordering
	// produced by StorageNodeSelector so volume selection starts from the
	// hottest node.
	byCluster := make(map[string]*clusterWork)
	for _, p := range nodePairs {
		cw := byCluster[p.ClusterUUID]
		if cw == nil {
			cw = &clusterWork{
				targetBySource:        make(map[string]string),
				targetClusterBySource: make(map[string]string),
			}
			byCluster[p.ClusterUUID] = cw
		}
		cw.hotNodes = append(cw.hotNodes, p.SourceNodeUUID)
		cw.targetBySource[p.SourceNodeUUID] = p.TargetNodeUUID
		cw.targetClusterBySource[p.SourceNodeUUID] = p.TargetClusterUUID
	}

	// Step 3 — for each cluster, collect volumes and select the migration set.
	for clusterUUID, cw := range byCluster {
		pinned, err := rb.volumeSelector.BuildPinnedSet(ctx, clusterUUID)
		if err != nil {
			return nil, false, fmt.Errorf("cluster %s: build pinned set: %w", clusterUUID, err)
		}

		namespaced, err := rb.volumeSelector.BuildNamespacedSet(ctx, clusterUUID)
		if err != nil {
			return nil, false, fmt.Errorf("cluster %s: build namespaced set: %w", clusterUUID, err)
		}

		lvInput := LogicalVolumeSelectorInput{
			ClusterUUID:   clusterUUID,
			PrometheusURL: cfg.PrometheusURL,
			Pinned:        pinned,
			Namespaced:    namespaced,
		}
		if isCoolingDown != nil {
			// Force capture clusterUUID for the closure.
			lvInput.IsCoolingDown = func(clusterUUID string) func(string) bool {
				return func(volumeUUID string) bool {
					return isCoolingDown(clusterUUID, volumeUUID)
				}
			}(clusterUUID)
		}

		volumesByNode, err := rb.volumeSelector.CollectVolumes(ctx, lvInput)
		if err != nil {
			return nil, false, fmt.Errorf("cluster %s: collect volumes: %w", clusterUUID, err)
		}

		sourceNodeUUID, toMigrate := rb.volumeSelector.SelectVolumesForMigration(lvInput, cw.hotNodes, volumesByNode, cfg)
		if sourceNodeUUID == "" {
			// No hot node yielded an eligible volume. Distinguish the pinned case:
			// a hot node that hosts volumes, every one of which is pinned, is blocked
			// from rebalancing by policy (not by cool-down or lack of load).
			if hotNodesAllPinned(cw.hotNodes, volumesByNode, pinned) {
				pinnedBlocked = true
			}
			continue
		}

		targetNodeUUID := cw.targetBySource[sourceNodeUUID]
		targetClusterUUID := cw.targetClusterBySource[sourceNodeUUID]
		for _, rc := range toMigrate {
			candidates = append(candidates, MigrationCandidate{
				ClusterUUID:       clusterUUID,
				SourceNodeUUID:    sourceNodeUUID,
				TargetClusterUUID: targetClusterUUID,
				TargetNodeUUID:    targetNodeUUID,
				Volume:            rc.Vol,
			})
		}
	}
	return candidates, pinnedBlocked, nil
}

// hotNodesAllPinned reports whether any hot node hosts at least one volume and
// every volume it hosts is pinned — i.e. the node is hot but rebalancing is
// blocked purely by pin policy. volumesByNode contains all volumes (pinning is
// applied downstream), so it is the correct set to test against pinned.
func hotNodesAllPinned(hotNodes []string, volumesByNode map[string][]VolumePlacement, pinned map[string]bool) bool {
	for _, node := range hotNodes {
		vols := volumesByNode[node]
		if len(vols) == 0 {
			continue
		}
		allPinned := true
		for _, vp := range vols {
			if !pinned[vp.UUID] {
				allPinned = false
				break
			}
		}
		if allPinned {
			return true
		}
	}
	return false
}
