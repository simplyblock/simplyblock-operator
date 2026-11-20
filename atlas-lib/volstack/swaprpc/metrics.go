// The node's swap metrics, as Prometheus collectors: how many volumes are
// holding I/O now, how long a namespace took to appear after a rescan, and how
// often the watchdog had to resume a volume nobody resumed
// (design-cg-live-subsystem-flip.md §12).

package swaprpc

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusMetrics is the Metrics a node plugin registers.
type PrometheusMetrics struct {
	suspended prometheus.Gauge
	rescan    prometheus.Observer
	watchdog  *prometheus.CounterVec
	node      string
}

// NewPrometheusMetrics registers the swap metrics for node with reg.
func NewPrometheusMetrics(reg prometheus.Registerer, node string) (*PrometheusMetrics, error) {
	suspended := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "simplyblock_csi_node_swap_suspended",
		Help: "Volumes whose device-mapper mapping is holding I/O for a subsystem swap.",
	}, []string{"node"})
	rescan := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "simplyblock_csi_node_swap_rescan_seconds",
		Help:    "Time from a resume's rescan to the namespace appearing under its new subsystem.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
	}, []string{"node"})
	watchdog := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "simplyblock_csi_node_swap_watchdog_total",
		Help: "Volumes the node resumed on its own after their deadline, by where it found the namespace.",
	}, []string{"node", "outcome"})
	for _, c := range []prometheus.Collector{suspended, rescan, watchdog} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return &PrometheusMetrics{
		suspended: suspended.WithLabelValues(node),
		rescan:    rescan.WithLabelValues(node),
		watchdog:  watchdog,
		node:      node,
	}, nil
}

// SetSuspended records how many volumes are holding I/O.
func (m *PrometheusMetrics) SetSuspended(n int) { m.suspended.Set(float64(n)) }

// ObserveRescan records how long a namespace took to appear.
func (m *PrometheusMetrics) ObserveRescan(d time.Duration) { m.rescan.Observe(d.Seconds()) }

// Watchdog counts one resolution by the watchdog.
func (m *PrometheusMetrics) Watchdog(outcome string) { m.watchdog.WithLabelValues(m.node, outcome).Inc() }
