// The time series this package exports for ObjectStore (design-objectstore.md
// §10), apart from metrics.go's bucket-provisioning ones: a different design
// and a different CRD. Named with the _state/_count suffix
// design-crd-model.md §7.12 requires of a gauge and
// TestEveryMetricIsNamedTheWayTheModelSays
// (internal/controllers/names_test.go) enforces, rather than the design
// document's own simplyblock_objectstore_phase /
// simplyblock_objectstore_ready_replicas, the same gap §10's bucket-phase
// gauge had before W-06 renamed it; the design document is updated to match
// rather than left to say a name this operator does not export.
//
// Published by Collector on a timer (collector.go), for the same reason the
// bucket phase gauge is: an inline update from Reconcile has nothing to clear
// a deleted ObjectStore's series with, and would report it in whatever phase
// or replica count it was last seen at forever.

package objectstore

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// 1 for the phase an ObjectStore is in, 0 for every other phase.
	objectStorePhaseState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_objectstore_phase_state",
			Help: "1 for the phase the ObjectStore is in, 0 for every other phase.",
		},
		[]string{"objectstore", "phase"},
	)

	// Mirrors status.readyReplicas, the load-bearing one: it is what an
	// alert on "fewer gateway pods than requested" reads.
	objectStoreReadyReplicasCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "simplyblock_objectstore_ready_replicas_count",
			Help: "status.readyReplicas: how many of the gateway's pods are actually serving.",
		},
		[]string{"objectstore"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(objectStorePhaseState, objectStoreReadyReplicasCount)
}

// resetObjectStoreMetrics clears every series of both gauges before a
// Collector pass rebuilds them, so a deleted ObjectStore's series does not
// report it in its last phase or replica count forever.
func resetObjectStoreMetrics() {
	objectStorePhaseState.Reset()
	objectStoreReadyReplicasCount.Reset()
}
