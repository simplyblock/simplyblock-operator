// The metrics the operator publishes about storage nodes and the operations
// performed against them.
//
// They exist because nothing in the operator measured how long a drain takes, how
// long a node is locked, or how often a maintenance window holds for a peer.
// Three of them answer questions nothing else in the design can:
//
//   - operation_active_state is the alert for a leaked lock. The release paths
//     are idempotent and run from three places precisely because a lock held by a
//     terminal operation would block the node forever, and a gauge is how that is
//     noticed rather than reported.
//   - drain_blocked_volumes_count turns the most common support question about a
//     stalled drain into a dashboard panel.
//   - operation_step_deadline_exceeded_total distinguishes an operation still
//     working from one that stopped.
//
// provisioning_duration_seconds is the one to watch when a cluster is being
// expanded, because nodeProvisioningBudget and the FoundationDB serialization of
// §4.2 mean the time to add ten workers is not ten times the time to add one, and
// nothing today says what it actually is.
//
// An operation's metrics are the entity's: a StorageNodeOps records against
// simplyblock_storagenode_operations_total rather than a subsystem of its own,
// because the question an operator asks is what has happened to a node and the
// action label already says which operation it was (design-crd-model.md §7.12).
// Every series carries `cluster`, so one dashboard covers a multi-cluster
// deployment.
//
// design-storagenode.md §13.2 is the specification.

package node

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// operationBuckets span the range a node operation actually takes: a suspend that
// returns in seconds, a restart in minutes, and a drain of a node holding a
// hundred large volumes in hours. A linear set would put every interesting
// operation in one bucket.
var operationBuckets = prometheus.ExponentialBuckets(1, 3, 10)

var (
	operationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagenode_operation_duration_seconds",
			Help:    "How long an operation took, from acquiring the node's lock to a terminal phase.",
			Buckets: operationBuckets,
		},
		[]string{"cluster", "action", "result"},
	)

	operationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_storagenode_operations_total",
			Help: "Operations that reached a terminal phase, by succeeded, failed, and aborted.",
		},
		[]string{"cluster", "action", "result"},
	)

	operationStepDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagenode_operation_step_duration_seconds",
			Help:    "How long one step of an operation took, which is where a slow operation is actually slow.",
			Buckets: operationBuckets,
		},
		[]string{"cluster", "action", "step"},
	)

	operationStepDeadlineExceededTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_storagenode_operation_step_deadline_exceeded_total",
			Help: "Steps that ran out of time, including those that expired while the operator was down.",
		},
		[]string{"cluster", "action", "step"},
	)

	operationLockWaitSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagenode_operation_lock_wait_seconds",
			Help:    "How long an operation spent Pending behind another operation's lock.",
			Buckets: operationBuckets,
		},
		[]string{"cluster", "action"},
	)

	operationActiveState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagenode_operation_active_state",
			Help: "1 while a node's status.activeOpsRef is set, so a lock held by a finished operation is visible.",
		},
		[]string{"cluster", "node"},
	)

	drainBlockedVolumesCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagenode_drain_blocked_volumes_count",
			Help: "Volumes blocking a drain, by pinned and unmanaged.",
		},
		[]string{"cluster", "reason"},
	)

	drainVolumesMigratedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_storagenode_drain_volumes_migrated_total",
			Help: "Volumes moved off a node by a drain, so a drain's rate is graphable against its total.",
		},
		[]string{"cluster"},
	)

	maintenanceHoldSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagenode_maintenance_hold_seconds",
			Help:    "How long a maintenance window held before it got its concurrency slot.",
			Buckets: operationBuckets,
		},
		[]string{"cluster"},
	)

	nodePhaseState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagenode_phase_state",
			Help: "1 for the node's current phase and 0 for the rest, so a node stuck in Provisioning is alertable.",
		},
		[]string{"cluster", "node", "phase"},
	)

	provisioningDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagenode_provisioning_duration_seconds",
			Help:    "How long a node took from object creation to online, which is what a node add costs.",
			Buckets: operationBuckets,
		},
		[]string{"cluster"},
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
		drainBlockedVolumesCount,
		drainVolumesMigratedTotal,
		maintenanceHoldSeconds,
		nodePhaseState,
		provisioningDurationSeconds,
	)
}
