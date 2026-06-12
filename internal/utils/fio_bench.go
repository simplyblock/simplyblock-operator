package utils

// FioBenchConfigMapName returns the deterministic ConfigMap name for a cluster.
// The operator writes per-hostname JSON arrays here; fio-probe reads them.
func FioBenchConfigMapName(clusterName string) string {
	return "simplyblock-fio-bench-" + clusterName
}

// FioBenchMetricsPort is the port on which the fio-bench-probe sidecar exposes
// Prometheus metrics.
const FioBenchMetricsPort = 9199