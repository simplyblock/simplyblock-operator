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
	// MaxDeviationPct is the highest p99 latency deviation from baseline across
	// all nodes in the cluster, expressed as a percentage.
	MaxDeviationPct float64
	// AvgDeviationPct is the mean p99 latency deviation across all measured nodes.
	AvgDeviationPct float64
	// HottestNodeUUID is the node with the highest deviation (migration source candidate).
	HottestNodeUUID string
	// CoolestNodeUUID is the node with the lowest deviation (preferred migration target).
	CoolestNodeUUID string
}

// nodeLatencyData holds the fio p99 latency measurements for one storage node,
// combined from the Prometheus current reading and the StorageNode CR baseline.
type nodeLatencyData struct {
	// clusterUUID identifies which cluster this node belongs to, used to group
	// nodes when computing per-cluster deviation statistics.
	clusterUUID string
	// baselineP99NS is the one-time fio p99 write latency (ns) recorded by the
	// baseline Job and stored in StorageNode.status.latencyMetrics. Zero until
	// the baseline Job has completed for this node.
	baselineP99NS int64
	// currentP99NS is the most recent fio p99 write latency (ns) scraped from
	// Prometheus via the fio-bench-probe sidecar.
	currentP99NS int64
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
// Both nodes belong to the same cluster.
type NodeMigrationPair struct {
	// ClusterUUID is the storage cluster that owns both nodes.
	ClusterUUID string
	// SourceNodeUUID is the hot node whose latency deviation exceeds the threshold.
	SourceNodeUUID string
	// TargetNodeUUID is the coolest node in the cluster, chosen as the migration destination.
	TargetNodeUUID string
}

// SelectStorageNodes returns source→target node pairs for all nodes that exceed
// the imbalance threshold. For each cluster, every hot node (deviation above
// cfg.ImbalanceThreshold) is paired with the coolest node in that cluster as
// the migration target. Pairs where source == target are skipped.
func (sns *StorageNodeSelector) SelectStorageNodes(
	ctx context.Context,
	cfg RebalancingConfig,
	inputs ...StorageNodeSelectorInput,
) ([]NodeMigrationPair, error) {
	deviations, statsByCluster, err := sns.computeLatencyDeviations(ctx, cfg.PrometheusURL, inputs...)
	if err != nil {
		return nil, err
	}

	// Build nodeUUID → clusterUUID from inputs so we can group deviations by cluster.
	nodeCluster := make(map[string]string)
	for _, input := range inputs {
		for _, node := range input.StorageNodes {
			if node.UUID != "" && node.ClusterUUID != "" {
				nodeCluster[node.UUID] = node.ClusterUUID
			}
		}
	}

	// Partition the flat deviations map into per-cluster sub-maps.
	clusterDeviations := make(map[string]map[string]float64)
	for nodeUUID, dev := range deviations {
		clusterUUID := nodeCluster[nodeUUID]
		if clusterUUID == "" {
			continue
		}
		if clusterDeviations[clusterUUID] == nil {
			clusterDeviations[clusterUUID] = make(map[string]float64)
		}
		clusterDeviations[clusterUUID][nodeUUID] = dev
	}

	// For each cluster pair every hot node with the cluster's coolest node.
	var pairs []NodeMigrationPair
	for clusterUUID, clusterDevMap := range clusterDeviations {
		stats, ok := statsByCluster[clusterUUID]
		if !ok {
			continue
		}
		for _, sourceUUID := range volumemigration.NodesAboveThreshold(clusterDevMap, cfg.ImbalanceThreshold) {
			if stats.CoolestNodeUUID == "" || stats.CoolestNodeUUID == sourceUUID {
				continue
			}
			pairs = append(pairs, NodeMigrationPair{
				ClusterUUID:    clusterUUID,
				SourceNodeUUID: sourceUUID,
				TargetNodeUUID: stats.CoolestNodeUUID,
			})
		}
	}
	return pairs, nil
}

// computeLatencyDeviations collects per-node latency from Prometheus and StorageNode CRs,
// computes deviation from baseline, emits Prometheus gauges, and returns the per-node
// deviation map and per-cluster aggregate statistics.
func (sns *StorageNodeSelector) computeLatencyDeviations(
	ctx context.Context,
	prometheusURL string,
	inputs ...StorageNodeSelectorInput,
) (deviations map[string]float64, statsByCluster map[string]ClusterDeviationStats, err error) {
	latencyByNode, err := sns.collectLatencyState(ctx, prometheusURL, inputs...)
	if err != nil {
		return nil, nil, err
	}
	deviations = make(map[string]float64, len(latencyByNode))
	for nodeUUID, ld := range latencyByNode {
		dev := volumemigration.ComputeLatencyDeviationPct(ld.baselineP99NS, ld.currentP99NS)
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
//   - current p99 write latency queried from Prometheus (written by the fio-bench-probe sidecar)
//   - baseline p99 write latency read from StorageNode CR status (set once by the baseline Job)
func (sns *StorageNodeSelector) collectLatencyState(
	ctx context.Context,
	prometheusURL string,
	inputs ...StorageNodeSelectorInput,
) (map[string]nodeLatencyData, error) {
	currentByNodeByCluster, err := sns.collectCurrentLatency(ctx, prometheusURL, inputs...)
	if err != nil {
		return nil, err
	}

	// Collect baselines from all distinct namespaces present in the inputs.
	baselineByNode := make(map[string]int64)
	for _, input := range inputs {
		for nodeUUID, baseline := range sns.readBaselineFromCRs(ctx, input.Namespace) {
			baselineByNode[nodeUUID] = baseline
		}
	}

	// Flatten [clusterUUID][nodeUUID] → int64 into nodeUUID → nodeLatencyData.
	result := make(map[string]nodeLatencyData)
	for clusterUUID, byNode := range currentByNodeByCluster {
		for nodeUUID, curr := range byNode {
			result[nodeUUID] = nodeLatencyData{
				baselineP99NS: baselineByNode[nodeUUID], // 0 until baseline Job completes
				currentP99NS:  curr,
				clusterUUID:   clusterUUID,
			}
		}
	}
	return result, nil
}

// collectCurrentLatency queries Prometheus for the most recent p99 write latency
// across all clusters referenced in inputs in a single round-trip, returning
// a map[clusterUUID][nodeUUID]p99NS.
func (sns *StorageNodeSelector) collectCurrentLatency(
	ctx context.Context,
	prometheusURL string,
	inputs ...StorageNodeSelectorInput,
) (map[string]map[string]int64, error) {
	provider, err := promlatency.New(prometheusURL)
	if err != nil {
		return nil, fmt.Errorf("create prometheus latency provider: %w", err)
	}

	clusterIds := distinctClusterUUIDs(inputs)
	return provider.GetClustersCurrentP99(ctx, clusterIds)
}

// readBaselineFromCRs returns a nodeUUID → BaselineP99NS map from all StorageNode CRs
// in the given namespace. The baseline is set exactly once by the one-shot baseline Job.
func (sns *StorageNodeSelector) readBaselineFromCRs(
	ctx context.Context,
	namespace string,
) map[string]int64 {
	result := make(map[string]int64)
	var snodeList simplyblockv1alpha1.StorageNodeList
	if err := sns.List(ctx, &snodeList, client.InNamespace(namespace)); err != nil {
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
