/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	rebalancerEvaluationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_rebalancer_evaluation_total",
			Help: "Total evaluation cycles, labelled by outcome (skipped, migrated, blocked, error).",
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
		rebalancerMaxLatencyDeviationPct,
		rebalancerNodeLatencyDeviationPct,
		rebalancerCooldownVolumes,
		rebalancerPinnedBlockedTotal,
	)
}
