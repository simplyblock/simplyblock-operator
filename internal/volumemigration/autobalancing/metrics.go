package autobalancing

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	rebalancerMaxLatencyDeviationPct = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_rebalancer_max_latency_deviation_pct",
			Help: "Maximum p99 write latency deviation from per-node baseline, in percent, across all storage nodes in the cluster.",
		},
		[]string{"cluster"},
	)

	rebalancerNodeLatencyDeviationPct = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_rebalancer_node_latency_deviation_pct",
			Help: "Per-node p99 write latency deviation from baseline, in percent (Phase 1). Will reflect weighted I/O score in Phase 2.",
		},
		[]string{"cluster", "node"},
	)

	rebalancerCooldownVolumes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_rebalancer_cooldown_volumes",
			Help: "Number of volumes currently in the post-migration cool-down window.",
		},
		[]string{"cluster"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		rebalancerMaxLatencyDeviationPct,
		rebalancerNodeLatencyDeviationPct,
		rebalancerCooldownVolumes,
	)
}

// SetCooldownVolumes updates the per-cluster cooldown-volume gauge.
// Called by the controller after each evaluation cycle.
func SetCooldownVolumes(clusterName string, count float64) {
	rebalancerCooldownVolumes.WithLabelValues(clusterName).Set(count)
}
