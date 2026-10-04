// The gauges the operator publishes about storage pools.
//
// They are declared apart from the collector that fills them because a gauge is
// process-global state registered once at init and a collector is a Runnable
// that may be constructed more than once. Keeping them separate is also what
// lets a test reset the series without reaching into the collector.
//
// A pool is labeled by its object name and its cluster's, rather than by the
// control plane's UUIDs. These gauges exist so a Kubernetes-side dashboard can
// name what it is showing, and the control plane's own exporter already
// publishes the same capacity under its ids for anybody who wants to join the
// two.
//
// design-storagepool.md §9.2 is the specification.

package pool

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// poolLabelNames are the labels every per-pool series carries.
var poolLabelNames = []string{"cluster", "pool"}

var (
	// poolCapacityBytes against poolUsedBytes is the pair a tenant operator
	// watches, and the only one in this group that answers a capacity-planning
	// question rather than a health one.
	poolCapacityBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagepool_capacity_bytes",
			Help: "The pool's capacity limit. Zero means the pool is unlimited.",
		},
		poolLabelNames,
	)

	poolUsedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagepool_used_bytes",
			Help: "Space the pool has allocated. Absent while no capacity sample is available.",
		},
		poolLabelNames,
	)

	poolProvisionedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagepool_provisioned_bytes",
			Help: "Space the pool's volumes were promised, which on a thin-provisioned pool exceeds what it holds.",
		},
		poolLabelNames,
	)

	// poolVolumesCount and poolBoundVolumesCount are the second integrity
	// signal: the control plane holding volumes Kubernetes does not account for
	// is the unmanaged-volume condition that blocks a node drain, and it is
	// better noticed before somebody tries to drain.
	poolVolumesCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagepool_volumes_count",
			Help: "Logical volumes the control plane reports in the pool.",
		},
		poolLabelNames,
	)

	poolBoundVolumesCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagepool_bound_volumes_count",
			Help: "PersistentVolume objects provisioned out of the pool, which is what holds its deletion.",
		},
		poolLabelNames,
	)

	// One series per phase rather than one carrying a phase number: a dashboard
	// graphing "how many pools are stuck deleting" should not have to know which
	// integer means Deleting, and a phase added later would renumber the rest.
	poolPhaseState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagepool_phase_state",
			Help: "1 for the phase the storage pool is in, 0 for every other phase.",
		},
		append(append([]string{}, poolLabelNames...), "phase"),
	)

	// The first integrity signal: a pool with no class is capacity nothing can
	// consume, which is a valid state for a fresh cluster and a broken one for
	// anything else.
	poolStorageClassesCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_storagepool_storageclasses_count",
			Help: "StorageClasses assigned to the pool, which is zero for a pool nothing consumes.",
		},
		poolLabelNames,
	)

	poolOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_storagepool_operations_total",
			Help: "StoragePoolOps reaching a terminal phase.",
		},
		[]string{"cluster", "pool", "action", "result"},
	)

	poolOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_storagepool_operation_duration_seconds",
			Help:    "How long a StoragePoolOps took to reach a terminal phase.",
			Buckets: prometheus.ExponentialBuckets(1, 4, 8),
		},
		[]string{"cluster", "pool", "action"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		poolCapacityBytes,
		poolUsedBytes,
		poolProvisionedBytes,
		poolVolumesCount,
		poolBoundVolumesCount,
		poolPhaseState,
		poolStorageClassesCount,
		poolOperationsTotal,
		poolOperationDuration,
	)
}

// resetPoolMetrics drops every gauge series. The collector calls it at the top
// of each pass, which is what makes a pool that went away stop being reported: a
// gauge nobody updates any more keeps its last value forever, and a dashboard
// that says a deleted pool is still half full is worse than no dashboard.
//
// The two operation series are deliberately not reset. A counter and a histogram
// are cumulative records of what happened, not statements about what currently
// exists, and resetting either would make a rate over them meaningless.
func resetPoolMetrics() {
	poolCapacityBytes.Reset()
	poolUsedBytes.Reset()
	poolProvisionedBytes.Reset()
	poolVolumesCount.Reset()
	poolBoundVolumesCount.Reset()
	poolPhaseState.Reset()
	poolStorageClassesCount.Reset()
}
