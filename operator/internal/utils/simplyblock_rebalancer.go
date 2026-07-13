package utils

// SimplyblockRebalancerConfigMapName returns the deterministic ConfigMap name for a cluster.
// The operator writes per-hostname JSON arrays here; simplyblock-rebalancer reads them.
func SimplyblockRebalancerConfigMapName(clusterName string) string {
	return "simplyblock-rebalancer-" + clusterName
}

// SimplyblockRebalancerMetricsPort is the port on which the simplyblock-rebalancer sidecar exposes
// Prometheus metrics.
const SimplyblockRebalancerMetricsPort = 9199
