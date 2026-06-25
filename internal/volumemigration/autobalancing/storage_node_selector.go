package autobalancing

import (
	"context"
	"fmt"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	promlatency "github.com/simplyblock/simplyblock-operator/internal/metrics/prometheus"
	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StorageNodeSelectorInput groups the storage nodes belonging to a single Kubernetes
// namespace. Multiple inputs can be passed to SelectStorageNodes when nodes span
// more than one namespace (e.g. multi-tenant deployments).
type StorageNodeSelectorInput struct {
	// Namespace is the Kubernetes namespace that owns these storage nodes.
	// Used to scope StorageNode CR lookups when reading baseline latency.
	Namespace string
	// StorageNodes is the list of storage nodes in this namespace to consider
	// for source/target selection.
	StorageNodes []volumemigration.StorageNode
}

// ClusterDeviationStats holds the per-cluster aggregate latency deviation statistics
// computed by deviationStats from a single evaluation cycle.
type ClusterDeviationStats struct {
	// MaxDeviationPct is the highest p50 latency deviation from baseline across
	// all nodes in the cluster, expressed as a percentage.
	MaxDeviationPct float64
	// AvgDeviationPct is the mean p50 latency deviation across all measured nodes.
	AvgDeviationPct float64
	// HottestNodeUUID is the node with the highest deviation (migration source candidate).
	HottestNodeUUID string
	// CoolestNodeUUID is the node with the lowest deviation (preferred migration target).
	CoolestNodeUUID string
}

// nodeLatencyData holds the fio write-latency measurements for one storage node at the
// configured percentile (p50 or p99), combined from the Prometheus current reading and
// the StorageNode CR baseline. p50 (median) is the default — it is stable, whereas p99
// is dominated by journal/EC/HA tail spikes that make the deviation signal noisy.
type nodeLatencyData struct {
	// clusterUUID identifies which cluster this node belongs to, used to group
	// nodes when computing per-cluster deviation statistics.
	clusterUUID string
	// baselineNS is the one-time fio write latency (ns) at the configured percentile,
	// recorded by the baseline Job and stored in StorageNode.status.latencyMetrics.
	// Zero until the baseline Job has completed for this node.
	baselineNS int64
	// currentNS is the most recent fio write latency (ns) at the configured percentile,
	// scraped from Prometheus via the fio-bench-probe sidecar.
	currentNS int64
}

// StorageNodeSelector evaluates per-node latency deviation and selects source/target
// node pairs for volume migration. It combines current latency from Prometheus with
// baseline latency from StorageNode CRs and emits per-node deviation gauges.
type StorageNodeSelector struct {
	client.Client
}

// NewStorageNodeSelector creates a StorageNodeSelector backed by the given Kubernetes client.
func NewStorageNodeSelector(
	k8sClient client.Client,
) *StorageNodeSelector {
	return &StorageNodeSelector{
		Client: k8sClient,
	}
}

// NodeMigrationPair is a source→target node pairing produced by SelectStorageNodes.
//
// Source and target are tracked with their own cluster UUIDs so the type can already
// express a cross-cluster migration. In this release the selector only ever pairs nodes
// within the same cluster (TargetClusterUUID == ClusterUUID); cross-cluster target
// selection is a follow-up — see isMigrationTargetEligible.
type NodeMigrationPair struct {
	// ClusterUUID is the storage cluster that owns the source node (and the volume).
	ClusterUUID string
	// SourceNodeUUID is the hot node whose latency deviation exceeds the threshold.
	SourceNodeUUID string
	// TargetClusterUUID is the storage cluster that owns the target node. Equal to
	// ClusterUUID today; may differ once cross-cluster migration is enabled.
	TargetClusterUUID string
	// TargetNodeUUID is the chosen migration destination — the coolest eligible node
	// that is at least cfg.MinHotColdDifferencePct cooler than the source.
	TargetNodeUUID string
}

// nodeRef is the unit of source/target selection: a node, its owning cluster, and its
// current latency deviation. The selection operates over a flat pool of nodeRefs so it
// is agnostic to cluster boundaries — eligibility (incl. same-cluster vs cross-cluster)
// is decided by isMigrationTargetEligible, the single seam to relax for cross-cluster.
type nodeRef struct {
	ClusterUUID  string
	NodeUUID     string
	DeviationPct float64
}

// SelectStorageNodes returns source→target node pairs for every node whose latency
// deviation exceeds cfg.ImbalanceThreshold. Each hot node is paired with the coolest
// eligible target that is at least cfg.MinHotColdDifferencePct percentage points cooler;
// when no such target exists the hot node produces no pair (migrating between
// near-equally-loaded nodes yields no benefit).
//
// Selection runs over a flat pool of all nodes regardless of cluster; the source/target
// cluster relationship is decided by isMigrationTargetEligible (intra-cluster only
// today, cross-cluster is a follow-up).
func (sns *StorageNodeSelector) SelectStorageNodes(
	ctx context.Context,
	cfg RebalancingConfig,
	inputs ...StorageNodeSelectorInput,
) ([]NodeMigrationPair, error) {
	// computeLatencyDeviations also emits the per-node / max deviation gauges as a side
	// effect; the per-cluster stats it returns are not needed for pairing here.
	deviations, _, err := sns.computeLatencyDeviations(ctx, cfg.PrometheusURL, cfg.LatencyPercentile, inputs...)
	if err != nil {
		return nil, err
	}

	// Build nodeUUID → clusterUUID from inputs, then a flat pool of node references.
	nodeCluster := make(map[string]string)
	for _, input := range inputs {
		for _, node := range input.StorageNodes {
			if node.UUID != "" && node.ClusterUUID != "" {
				nodeCluster[node.UUID] = node.ClusterUUID
			}
		}
	}
	var nodes []nodeRef
	for nodeUUID, dev := range deviations {
		clusterUUID := nodeCluster[nodeUUID]
		if clusterUUID == "" {
			continue
		}
		nodes = append(nodes, nodeRef{ClusterUUID: clusterUUID, NodeUUID: nodeUUID, DeviationPct: dev})
	}

	// Pair every hot node with the coolest eligible, sufficiently-cooler target.
	var pairs []NodeMigrationPair
	for _, src := range nodes {
		if src.DeviationPct < cfg.ImbalanceThreshold {
			continue
		}
		target, ok := pickColdTarget(src, nodes, cfg)
		if !ok {
			continue
		}
		pairs = append(pairs, NodeMigrationPair{
			ClusterUUID:       src.ClusterUUID,
			SourceNodeUUID:    src.NodeUUID,
			TargetClusterUUID: target.ClusterUUID,
			TargetNodeUUID:    target.NodeUUID,
		})
	}
	return pairs, nil
}

// pickColdTarget returns the coolest eligible migration target for the hot source from
// the flat node pool, or ok=false when none qualifies. A candidate qualifies when it is
// eligible (isMigrationTargetEligible) and at least cfg.MinHotColdDifferencePct
// percentage points cooler than the source.
func pickColdTarget(src nodeRef, pool []nodeRef, cfg RebalancingConfig) (nodeRef, bool) {
	var best nodeRef
	found := false
	for _, cand := range pool {
		if cand.NodeUUID == src.NodeUUID {
			continue
		}
		if !isMigrationTargetEligible(src, cand, cfg) {
			continue
		}
		if src.DeviationPct-cand.DeviationPct < cfg.MinHotColdDifferencePct {
			continue
		}
		if !found || cand.DeviationPct < best.DeviationPct {
			best = cand
			found = true
		}
	}
	return best, found
}

// isMigrationTargetEligible reports whether cand may receive a volume migrated from src.
// Migration is intra-cluster only today, so the target must be in the source's cluster.
// Cross-cluster migration is a planned follow-up: relaxing this predicate (e.g. gated by
// a future cfg flag) is the single change needed here to allow cross-cluster targets.
func isMigrationTargetEligible(src, cand nodeRef, _ RebalancingConfig) bool {
	return cand.ClusterUUID == src.ClusterUUID
}

// computeLatencyDeviations collects per-node latency from Prometheus and StorageNode CRs,
// computes deviation from baseline, emits Prometheus gauges, and returns the per-node
// deviation map and per-cluster aggregate statistics.
func (sns *StorageNodeSelector) computeLatencyDeviations(
	ctx context.Context,
	prometheusURL string,
	percentile string,
	inputs ...StorageNodeSelectorInput,
) (deviations map[string]float64, statsByCluster map[string]ClusterDeviationStats, err error) {
	latencyByNode, err := sns.collectLatencyState(ctx, prometheusURL, percentile, inputs...)
	if err != nil {
		return nil, nil, err
	}
	deviations = make(map[string]float64, len(latencyByNode))
	for nodeUUID, ld := range latencyByNode {
		dev := volumemigration.ComputeLatencyDeviationPct(ld.baselineNS, ld.currentNS)
		rebalancerNodeLatencyDeviationPct.WithLabelValues(ld.clusterUUID, nodeUUID).Set(dev)
		deviations[nodeUUID] = dev
	}
	statsByCluster = deviationStats(latencyByNode, deviations)
	for clusterUUID, stats := range statsByCluster {
		rebalancerMaxLatencyDeviationPct.WithLabelValues(clusterUUID).Set(stats.MaxDeviationPct)
	}
	return deviations, statsByCluster, nil
}

// collectLatencyState builds a nodeUUID → nodeLatencyData map by combining:
//   - current p50 write latency queried from Prometheus (written by the fio-bench-probe sidecar)
//   - baseline p50 write latency read from StorageNode CR status (set once by the baseline Job)
func (sns *StorageNodeSelector) collectLatencyState(
	ctx context.Context,
	prometheusURL string,
	percentile string,
	inputs ...StorageNodeSelectorInput,
) (map[string]nodeLatencyData, error) {
	currentByNodeByCluster, err := sns.collectCurrentLatency(ctx, prometheusURL, percentile, inputs...)
	if err != nil {
		return nil, err
	}

	// Collect baselines from all distinct namespaces present in the inputs.
	baselineByNode := make(map[string]int64)
	for _, input := range inputs {
		for nodeUUID, baseline := range sns.readBaselineFromCRs(ctx, input.Namespace, percentile) {
			baselineByNode[nodeUUID] = baseline
		}
	}

	// Flatten [clusterUUID][nodeUUID] → int64 into nodeUUID → nodeLatencyData.
	result := make(map[string]nodeLatencyData)
	for clusterUUID, byNode := range currentByNodeByCluster {
		for nodeUUID, curr := range byNode {
			result[nodeUUID] = nodeLatencyData{
				baselineNS:  baselineByNode[nodeUUID], // 0 until baseline Job completes
				currentNS:   curr,
				clusterUUID: clusterUUID,
			}
		}
	}
	return result, nil
}

// collectCurrentLatency queries Prometheus for the most recent write latency at the
// configured percentile across all clusters referenced in inputs in a single
// round-trip, returning a map[clusterUUID][nodeUUID]latencyNS.
func (sns *StorageNodeSelector) collectCurrentLatency(
	ctx context.Context,
	prometheusURL string,
	percentile string,
	inputs ...StorageNodeSelectorInput,
) (map[string]map[string]int64, error) {
	provider, err := promlatency.New(prometheusURL)
	if err != nil {
		return nil, fmt.Errorf("create prometheus latency provider: %w", err)
	}

	clusterIds := distinctClusterUUIDs(inputs)
	return provider.GetClustersCurrentLatency(ctx, clusterIds, percentile)
}

// readBaselineFromCRs returns a nodeUUID → baseline-latency map (at the configured
// percentile) from all StorageNode CRs in the given namespace. The baseline is set
// exactly once by the one-shot baseline Job.
func (sns *StorageNodeSelector) readBaselineFromCRs(
	ctx context.Context,
	namespace string,
	percentile string,
) map[string]int64 {
	result := make(map[string]int64)
	var snodeList simplyblockv1alpha1.StorageNodeList
	if err := sns.List(ctx, &snodeList, client.InNamespace(namespace)); err != nil {
		return result
	}
	for _, snode := range snodeList.Items {
		for _, lm := range snode.Status.LatencyMetrics {
			baseline := lm.BaselineP50NS
			if percentile == promlatency.PercentileP99 {
				baseline = lm.BaselineP99NS
			}
			if baseline > 0 {
				result[lm.NodeUUID] = baseline
			}
		}
	}
	return result
}

// distinctClusterUUIDs returns the set of unique ClusterUUIDs present across
// all StorageNodes in the given inputs. Order is not guaranteed.
func distinctClusterUUIDs(inputs []StorageNodeSelectorInput) []string {
	seen := make(map[string]struct{})
	for _, input := range inputs {
		for _, node := range input.StorageNodes {
			if node.ClusterUUID != "" {
				seen[node.ClusterUUID] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for uuid := range seen {
		out = append(out, uuid)
	}
	return out
}

// deviationStats groups nodes by cluster and computes per-cluster aggregate
// latency deviation statistics. latencyByNode supplies the clusterUUID for each
// node; deviations supplies the pre-computed deviation percentage per node.
// Nodes present in deviations but absent from latencyByNode are ignored.
func deviationStats(
	latencyByNode map[string]nodeLatencyData,
	deviations map[string]float64,
) map[string]ClusterDeviationStats {
	// accumulator collects running stats for one cluster.
	type accumulator struct {
		sum     float64
		count   int
		maxDev  float64
		minDev  float64
		hottest string // nodeUUID with highest deviation
		coolest string // nodeUUID with lowest deviation
		first   bool   // sentinel to initialise min/max on first sample
	}
	acc := make(map[string]*accumulator)

	for nodeUUID, dev := range deviations {
		ld, ok := latencyByNode[nodeUUID]
		if !ok {
			continue
		}
		a, exists := acc[ld.clusterUUID]
		if !exists {
			a = &accumulator{first: true}
			acc[ld.clusterUUID] = a
		}
		a.sum += dev
		a.count++
		if a.first || dev > a.maxDev {
			a.maxDev = dev
			a.hottest = nodeUUID
		}
		if a.first || dev < a.minDev {
			a.minDev = dev
			a.coolest = nodeUUID
		}
		a.first = false
	}

	out := make(map[string]ClusterDeviationStats, len(acc))
	for clusterUUID, a := range acc {
		var avg float64
		if a.count > 0 {
			avg = a.sum / float64(a.count)
		}
		out[clusterUUID] = ClusterDeviationStats{
			MaxDeviationPct: a.maxDev,
			AvgDeviationPct: avg,
			HottestNodeUUID: a.hottest,
			CoolestNodeUUID: a.coolest,
		}
	}
	return out
}
