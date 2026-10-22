// The placement service's metrics (design-superclusters.md §13.2): how many
// placements each member received, and how long a decision took. The first is
// how a skewed strategy is seen, and the second the alert for a slow member,
// since the name lookup in every member sits on the CreateVolume path.

package placement

import (
	rand "math/rand/v2"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The results a decision is labeled with when no member is named.
const (
	resultPlaced    = "placed"
	resultRecorded  = "recorded"
	resultExisting  = "existing"
	resultGroup     = "group"
	resultExhausted = "exhausted"
	resultFailed    = "failed"
)

var (
	placementDecisions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "simplyblock_supercluster_placement_decisions_total",
			Help: "CreateVolume placements in a supercluster by chosen member, or exhausted when no member had room.",
		},
		[]string{"namespace", "supercluster", "cluster", "result"},
	)
	placementDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "simplyblock_supercluster_placement_duration_seconds",
			Help:    "Time to choose a member for a new volume, the name lookup and capacity reads included.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
		},
		[]string{"namespace", "supercluster"},
	)
)

func init() {
	metrics.Registry.MustRegister(placementDecisions, placementDuration)
}

// observeDecision counts one decision and its duration.
func observeDecision(
	supercluster *simplyblockv1alpha2.StorageCluster,
	member *simplyblockv1alpha2.StorageCluster,
	result string,
	took time.Duration,
) {
	cluster := ""
	if member != nil {
		cluster = member.Name
	}
	placementDecisions.WithLabelValues(supercluster.Namespace, supercluster.Name, cluster, result).Inc()
	placementDuration.WithLabelValues(supercluster.Namespace, supercluster.Name).Observe(took.Seconds())
}

// randomDraw picks one of n members that rank equal.
func randomDraw(n int) int { return rand.IntN(n) } //nolint:gosec // a tie-break, not a secret
