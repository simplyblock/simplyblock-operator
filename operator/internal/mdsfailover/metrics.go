// The takeover's metrics (design-pnfs-mds-standby.md §10).
//
// They register in controller-runtime's registry, as every other metric of the
// operator does, under the simplyblock_pnfs_mds_ prefix of the metadata
// server's existing gauges. A registry is a parameter so a test gathers its
// own.

package mdsfailover

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// labelCluster is the label every series carries.
const labelCluster = "cluster"

// The phases of simplyblock_pnfs_mds_takeover_seconds.
const (
	PhaseDetect     = "Detect"
	PhaseFence      = "Fence"
	PhaseStart      = "Start"
	PhaseReassemble = "Reassemble"
	PhaseServe      = "Serve"
)

// The results of simplyblock_pnfs_mds_failovers_total.
const (
	ResultSucceeded   = "Succeeded"
	ResultFenceFailed = "FenceFailed"
	ResultConflict    = "Conflict"
)

// roleNames are the values of the role label, in the order they are published.
var roleNames = []nfsexportrpc.Role{nfsexportrpc.RoleActive, nfsexportrpc.RoleStandby, nfsexportrpc.RolePromoting}

// Metrics is the four series of a takeover.
type Metrics struct {
	role      *prometheus.GaugeVec
	epoch     *prometheus.GaugeVec
	failovers *prometheus.CounterVec
	takeover  *prometheus.HistogramVec

	mu    sync.Mutex
	known map[string]map[string]struct{}
}

// NewMetrics registers the series with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		role: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "simplyblock_pnfs_mds_role_state",
			Help: "One for the role each metadata server guest reports, and zero for the others.",
		}, []string{labelCluster, "pod", "role"}),
		epoch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "simplyblock_pnfs_mds_epoch_state",
			Help: "The active record's current epoch.",
		}, []string{labelCluster}),
		failovers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simplyblock_pnfs_mds_failovers_total",
			Help: "Takeovers by cause (Deleted, Lost, First) and result (Succeeded, FenceFailed, Conflict).",
		}, []string{labelCluster, "cause", "result"}),
		takeover: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "simplyblock_pnfs_mds_takeover_seconds",
			Help:    "Seconds each phase of a takeover took: Detect, Fence, Start, Reassemble, Serve.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 45, 60, 120},
		}, []string{labelCluster, "phase"}),
		known: map[string]map[string]struct{}{},
	}
	reg.MustRegister(m.role, m.epoch, m.failovers, m.takeover)
	return m
}

var (
	defaultMetrics     *Metrics
	defaultMetricsOnce sync.Once
)

// DefaultMetrics is the set registered in controller-runtime's registry.
func DefaultMetrics() *Metrics {
	defaultMetricsOnce.Do(func() { defaultMetrics = NewMetrics(ctrlmetrics.Registry) })
	return defaultMetrics
}

// SetRoles publishes the role each guest reports, and drops the series of a pod
// that is no longer there. A role of 0 is a guest that did not answer.
func (m *Metrics) SetRoles(cluster string, roles map[string]nfsexportrpc.Role) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for pod := range m.known[cluster] {
		if _, still := roles[pod]; !still {
			for _, r := range roleNames {
				m.role.DeleteLabelValues(cluster, pod, r.String())
			}
		}
	}
	seen := make(map[string]struct{}, len(roles))
	for pod, role := range roles {
		seen[pod] = struct{}{}
		for _, r := range roleNames {
			v := 0.0
			if r == role {
				v = 1
			}
			m.role.WithLabelValues(cluster, pod, r.String()).Set(v)
		}
	}
	m.known[cluster] = seen
}

// SetEpoch publishes the record's epoch.
func (m *Metrics) SetEpoch(cluster string, epoch uint64) {
	m.epoch.WithLabelValues(cluster).Set(float64(epoch))
}

// Failover counts one takeover's outcome.
func (m *Metrics) Failover(cluster string, cause Cause, result string) {
	m.failovers.WithLabelValues(cluster, string(cause), result).Inc()
}

// Phase observes how long one phase of a takeover took.
func (m *Metrics) Phase(cluster, phase string, d time.Duration) {
	m.takeover.WithLabelValues(cluster, phase).Observe(d.Seconds())
}
