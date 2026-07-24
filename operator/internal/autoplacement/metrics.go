package autoplacement

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

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

	rebalancerBaselineNS = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_rebalancer_baseline_ns",
			Help: "Per-node write latency baseline (ns) used as the deviation denominator, as resolved by the configured baseline strategy (at the operator-configured percentile, p50 or p99).",
		},
		[]string{"cluster", "node"},
	)

	rebalancerBaselineSamplesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_rebalancer_baseline_samples_total",
			Help: "Number of latency samples in the rolling window from which the per-node baseline was computed (rollingWindow strategy only).",
		},
		[]string{"cluster", "node"},
	)

	rebalancerBaselineSamplesRejected = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_rebalancer_baseline_samples_rejected",
			Help: "Number of rolling-window latency samples rejected as outliers by the Hampel identifier when computing the per-node baseline (rollingWindow strategy only).",
		},
		[]string{"cluster", "node"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		rebalancerMaxLatencyDeviationPct,
		rebalancerNodeLatencyDeviationPct,
		rebalancerCooldownVolumes,
		rebalancerBaselineNS,
		rebalancerBaselineSamplesTotal,
		rebalancerBaselineSamplesRejected,
	)
}

// setBaselineGauges records the computed per-node baseline and its rolling-window sample
// diagnostics (total considered / rejected as outliers). Called by the rolling-window
// baseline provider for each node it resolves.
func setBaselineGauges(clusterUUID, nodeUUID string, baselineNS int64, samplesTotal, samplesRejected int) {
	rebalancerBaselineNS.WithLabelValues(clusterUUID, nodeUUID).Set(float64(baselineNS))
	rebalancerBaselineSamplesTotal.WithLabelValues(clusterUUID, nodeUUID).Set(float64(samplesTotal))
	rebalancerBaselineSamplesRejected.WithLabelValues(clusterUUID, nodeUUID).Set(float64(samplesRejected))
}

// SetCooldownVolumes updates the per-cluster cooldown-volume gauge.
// Called by the controller after each evaluation cycle.
func SetCooldownVolumes(clusterUUID string, count float64) {
	rebalancerCooldownVolumes.WithLabelValues(clusterUUID).Set(count)
}
