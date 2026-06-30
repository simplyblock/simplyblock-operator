package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
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
		rebalancerEvaluationTotal,
		rebalancerMigrationsTotal,
		rebalancerPinnedBlockedTotal,
	)
}
