// The time series a volume operation exports.
//
// They are declared apart from the controller that fills them for the reason
// every metrics file in this repository is: a collector is process-global state
// registered once at init, while a reconciler is constructed per manager and
// may be constructed more than once in a test.
//
// The one that would have found the production defect is the per-step
// histogram. A migration whose creation is fast and whose copy is fast, and
// which takes half an hour, is a migration spending its time somewhere the
// registered kind's merged phase enum could not name.
//
// design-persistentvolumeops.md §8.2 is the specification, and
// design-crd-model.md §7.12 is the naming rule the suffixes come from.

package volume

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// migrationBuckets span a second to roughly half a day. A migration of an idle
// single-volume subsystem completes in seconds, and one of a large
// many-member subsystem runs for hours, and one set has to hold both.
var migrationBuckets = prometheus.ExponentialBuckets(1, 4, 9)

var (
	operationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_persistentvolume_operations_total",
			Help: "Volume operations that reached a terminal phase, by action and result.",
		},
		[]string{"cluster", "action", "result"},
	)

	operationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_persistentvolume_operation_duration_seconds",
			Help:    "How long a volume operation took from creation to a terminal phase.",
			Buckets: migrationBuckets,
		},
		[]string{"cluster", "action"},
	)

	// Where a slow migration is actually slow, which the registered kind's
	// merged phase enum could not say.
	stepDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_persistentvolume_operation_step_duration_seconds",
			Help:    "How long each step of a volume operation took.",
			Buckets: migrationBuckets,
		},
		[]string{"cluster", "action", "step"},
	)

	stepDeadlinesExceeded = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_persistentvolume_operation_step_deadline_exceeded_total",
			Help: "Steps of a volume operation that ran out of time.",
		},
		[]string{"cluster", "action", "step"},
	)

	// How long operations wait behind another operation on the same volume,
	// which is only expressible because the volume carries a real lock.
	operationQueuedSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_persistentvolume_operation_queued_seconds",
			Help:    "How long a volume operation was held behind another on the same volume.",
			Buckets: migrationBuckets,
		},
		[]string{"cluster"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		operationsTotal,
		operationDuration,
		stepDuration,
		stepDeadlinesExceeded,
		operationQueuedSeconds,
	)
}

// observeOperation records a terminal operation, how long it took, and how long
// it spent waiting for the volume before it started.
//
// An operation that never started has no duration to report, and reporting zero
// would drag the distribution toward a value nothing took.
func (r *PersistentVolumeOpsReconciler) observeOperation(
	ops *simplyblockv1alpha2.PersistentVolumeOps,
	phase simplyblockv1alpha2.PersistentVolumeOpsPhase,
) {
	cluster, action := clusterOf(ops), string(ops.Spec.Action)
	operationsTotal.WithLabelValues(cluster, action, resultOf(phase)).Inc()

	if started := ops.Status.StartedAt; started != nil {
		operationDuration.WithLabelValues(cluster, action).
			Observe(time.Since(started.Time).Seconds())
		if deferred := ops.Status.DeferredSince; deferred != nil {
			operationQueuedSeconds.WithLabelValues(cluster).
				Observe(started.Sub(deferred.Time).Seconds())
		}
	}
}

// observeStep records how long the step that just finished took.
func (r *PersistentVolumeOpsReconciler) observeStep(
	ops *simplyblockv1alpha2.PersistentVolumeOps, subject *subject, current step,
) {
	started, known := stepStarted(ops, current)
	if !known {
		return
	}
	stepDuration.WithLabelValues(subject.clusterUUID, string(ops.Spec.Action), string(current)).
		Observe(time.Since(started).Seconds())
}

// clusterOf is the cluster label, which is empty for an operation that failed
// before it could resolve one. That is a value worth having rather than
// dropping: an operation that failed for that reason is exactly the kind
// somebody wants a rate of.
func clusterOf(ops *simplyblockv1alpha2.PersistentVolumeOps) string {
	if ops.Status.Migration == nil {
		return ""
	}
	return ops.Status.Migration.ClusterUUID
}

// resultOf is the metric label for a terminal phase, lowercased because a label
// value is not an API enum.
func resultOf(phase simplyblockv1alpha2.PersistentVolumeOpsPhase) string {
	switch phase {
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded:
		return "succeeded"
	case simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted:
		return "aborted"
	default:
		return "failed"
	}
}
