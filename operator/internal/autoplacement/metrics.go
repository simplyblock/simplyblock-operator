package autoplacement

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// What rebalancing exports. The gauges describe the state the last evaluation cycle
// observed; the counters describe what it decided to do about it.
var (
	rebalancerMaxLatencyDeviationPct = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_rebalancer_max_latency_deviation_pct",
			Help: "Maximum write latency deviation from per-node baseline, in percent, across all storage nodes in the cluster (at the operator-configured percentile, p50 or p99).",
		},
		[]string{"cluster"},
	)

	rebalancerNodeLatencyDeviationPct = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_rebalancer_node_latency_deviation_pct",
			Help: "Per-node write latency deviation from baseline, in percent (at the operator-configured percentile, p50 or p99).",
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

	rebalancerEvaluationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_rebalancer_evaluation_total",
			Help: "Total evaluation cycles, labelled by outcome (skipped, migrated, dry_run, error).",
		},
		[]string{"cluster", "result"},
	)

	rebalancerMigrationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_rebalancer_migrations_total",
			Help: "Total volume migrations initiated.",
		},
		[]string{"cluster", "source_node", "target_node"},
	)

	rebalancerPinnedBlockedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_rebalancer_pinned_blocked_total",
			Help: "Number of times rebalancing was blocked because all hot volumes are pinned.",
		},
		[]string{"cluster"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		rebalancerMaxLatencyDeviationPct,
		rebalancerNodeLatencyDeviationPct,
		rebalancerCooldownVolumes,
		rebalancerEvaluationTotal,
		rebalancerMigrationsTotal,
		rebalancerPinnedBlockedTotal,
	)
}

// SetCooldownVolumes updates the per-cluster cooldown-volume gauge.
// Called by the controller after each evaluation cycle.
func SetCooldownVolumes(clusterUUID string, count float64) {
	rebalancerCooldownVolumes.WithLabelValues(clusterUUID).Set(count)
}
