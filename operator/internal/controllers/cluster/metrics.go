// The metrics the operator publishes about storage clusters and the operations
// performed against them.
//
// They exist because an operation that walks twenty nodes and holds the
// cluster's lock for an hour has to be measurable as more than a changing
// status.message. Three of them answer questions nothing else in the design
// can:
//
//   - operation_active_state is the alert for a leaked lock. The release paths
//     are idempotent and run from three places precisely because a lock held by
//     a terminal operation would block the cluster forever, and a gauge is how
//     that is noticed rather than reported.
//   - rolling_restart_peer_hold_seconds makes the peer hold a measurement
//     rather than an anecdote, which is what decides whether it should
//     eventually carry a timeout.
//   - operation_step_deadline_exceeded_total distinguishes an operation still
//     working from one that stopped.
//
// Every series carries `cluster`, matching the rebalancer's existing
// convention, so one dashboard covers a multi-cluster deployment.
//
// design-storagecluster.md §10.2 is the specification.

package cluster

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// operationBuckets span the range a cluster operation actually takes: an
// activate that returns in seconds, a restart in minutes, and a rolling restart
// over a large fleet in hours. A linear set would put every interesting
// operation in one bucket.
var operationBuckets = prometheus.ExponentialBuckets(1, 3, 10)

var (
	operationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagecluster_operation_duration_seconds",
			Help:    "How long an operation took, from acquiring the cluster's lock to a terminal phase.",
			Buckets: operationBuckets,
		},
		[]string{"cluster", "action", "result"},
	)

	operationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_storagecluster_operations_total",
			Help: "Operations that reached a terminal phase, by succeeded, failed, and aborted.",
		},
		[]string{"cluster", "action", "result"},
	)

	operationStepDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagecluster_operation_step_duration_seconds",
			Help:    "How long one step of an operation took, which is where a slow operation is actually slow.",
			Buckets: operationBuckets,
		},
		[]string{"cluster", "action", "step"},
	)

	operationStepDeadlineExceededTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_storagecluster_operation_step_deadline_exceeded_total",
			Help: "Steps that ran out of time, including those that expired while the operator was down.",
		},
		[]string{"cluster", "action", "step"},
	)

	operationLockWaitSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagecluster_operation_lock_wait_seconds",
			Help:    "How long an operation spent Pending behind another operation's lock.",
			Buckets: operationBuckets,
		},
		[]string{"cluster", "action"},
	)

	operationActiveState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagecluster_operation_active_state",
			Help: "1 while the cluster's status.activeOpsRef is set, so a lock held by a finished operation is visible.",
		},
		[]string{"cluster"},
	)

	rollingRestartPeerHoldSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagecluster_rolling_restart_peer_hold_seconds",
			Help:    "How long a rolling restart held before a node, waiting for its peers to come back online.",
			Buckets: operationBuckets,
		},
		[]string{"cluster"},
	)

	rollingRestartNodeIndex = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagecluster_rolling_restart_node_index_count",
			Help: "The walk's position in its node list, against rolling_restart_node_count, so progress is graphable.",
		},
		[]string{"cluster"},
	)

	rollingRestartNodeCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagecluster_rolling_restart_node_count",
			Help: "How many nodes the running walk covers. Absent when no rolling restart is in flight.",
		},
		[]string{"cluster"},
	)

	clusterPhaseState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagecluster_phase_state",
			Help: "1 for the cluster's current phase and 0 for the rest, so a cluster stuck in Creating is alertable.",
		},
		[]string{"cluster", "phase"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		operationDurationSeconds,
		operationsTotal,
		operationStepDurationSeconds,
		operationStepDeadlineExceededTotal,
		operationLockWaitSeconds,
		operationActiveState,
		rollingRestartPeerHoldSeconds,
		rollingRestartNodeIndex,
		rollingRestartNodeCount,
		clusterPhaseState,
	)
}
