// The node's swap metrics under the names design-cg-live-subsystem-flip.md §12
// gives them.

package swaprpc

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestTheSwapMetricsAreExportedUnderTheirDesignedNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewPrometheusMetrics(reg, "worker-2")
	if err != nil {
		t.Fatal(err)
	}
	m.SetSuspended(3)
	m.ObserveRescan(750 * time.Millisecond)
	m.Watchdog(OutcomeSource)
	m.Watchdog(OutcomeSource)

	if got := testutil.ToFloat64(m.suspended); got != 3 {
		t.Errorf("suspended = %v", got)
	}
	if got := testutil.ToFloat64(m.watchdog.WithLabelValues("worker-2", OutcomeSource)); got != 2 {
		t.Errorf("watchdog{source} = %v", got)
	}
	names, err := testutil.GatherAndCount(reg,
		"simplyblock_csi_node_swap_suspended",
		"simplyblock_csi_node_swap_rescan_seconds",
		"simplyblock_csi_node_swap_watchdog_total")
	if err != nil || names != 3 {
		t.Fatalf("gathered %d series (%v), want all three metrics", names, err)
	}
}

func TestRegisteringTheSwapMetricsTwiceFails(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := NewPrometheusMetrics(reg, "n"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPrometheusMetrics(reg, "n"); err == nil {
		t.Fatal("a second registration was accepted")
	}
}
