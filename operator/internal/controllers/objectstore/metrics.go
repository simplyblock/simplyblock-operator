// The time series this package exports (design-bucket-provisioning.md §10).
//
// Declared apart from the reconciler and the collector that fill them, the
// reason every metrics file in this repository is: a collector is
// process-global state registered once at init, while a reconciler (and a
// Collector) is constructed per manager and may be constructed more than
// once in a test.
//
// bucketProvisioningSeconds is the load-bearing one: it is the only way to
// learn, from a running cluster rather than a guess, whether the gateway's
// synchronous CreateBucket wait bound (design §12 Open Question 4) is set to
// a number that reflects reality.

package objectstore

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// bucketProvisioningBuckets spans a tenth of a second to roughly fifteen
// minutes in powers of two, the range a bucket's first-ever provisioning
// (cold MDS) and a later one (warm MDS) both fall in.
var bucketProvisioningBuckets = prometheus.ExponentialBuckets(0.1, 2, 14)

var (
	// 1 for the phase a bucket is in, 0 for every other phase. Published by
	// Collector on a timer rather than inline in Reconcile, for the same
	// reason the storage pool's own phase gauge is: an inline update has
	// nothing to clear a deleted bucket's series with, and would report it
	// in whatever phase it was last seen in forever.
	bucketPhaseState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_objectstore_bucket_phase_state",
			Help: "1 for the phase the bucket is in, 0 for every other phase.",
		},
		[]string{"bucket", "objectstore", "phase"},
	)

	// The bucket's own CreationTimestamp to the moment it reaches Ready,
	// observed once, from Reconcile itself: Ready has no outgoing edge, so
	// the transition into it runs exactly once per bucket (see
	// s3bucket_controller.go's own note beside where this is observed).
	bucketProvisioningSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_objectstore_bucket_provisioning_seconds",
			Help:    "Time from a bucket's creation to Ready.",
			Buckets: bucketProvisioningBuckets,
		},
		[]string{"objectstore"},
	)

	// One DeliverBucket/WithdrawBucket call outcome, by node and result.
	mountDeliveryCallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_objectstore_mount_delivery_calls_total",
			Help: "DeliverBucket/WithdrawBucket calls, by node, operation, and outcome.",
		},
		[]string{"objectstore", "node", "operation", "outcome"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(bucketPhaseState, bucketProvisioningSeconds, mountDeliveryCallsTotal)
}

// resetBucketPhaseMetrics clears every series before a Collector pass
// rebuilds them, so a deleted bucket's series does not report it in its last
// phase forever.
func resetBucketPhaseMetrics() {
	bucketPhaseState.Reset()
}
