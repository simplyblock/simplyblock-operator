// The HostMaintenance window's metrics: the five series are registered under
// the names design-fdb-host-maintenance.md §11 promises, and the zone gauge
// reads zero once a window that set the zone has ended.

package controlplane

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func TestTheWindowsSeriesAreRegistered(t *testing.T) {
	// A vector with no child is gathered only once it has one, so touch each.
	fdbMaintenanceZoneActive.WithLabelValues("ns", "w").Set(0)
	fdbEvictionHoldSeconds.WithLabelValues("ns", "Sentinel").Observe(1)
	fdbHoldingWindows.WithLabelValues("ns").Set(0)
	fdbCommandDurationSeconds.WithLabelValues("ns", "coordinators", "Complete").Observe(1)
	fdbCoordinatorMovesTotal.WithLabelValues("ns", "Spread").Add(0)
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{
		"simplyblock_controlplane_fdb_maintenance_zone_state",
		"simplyblock_controlplane_fdb_eviction_hold_seconds",
		"simplyblock_controlplane_fdb_holding_windows_count",
		"simplyblock_controlplane_fdb_command_duration_seconds",
		"simplyblock_controlplane_fdb_coordinator_moves_total",
	} {
		if !names[want] {
			t.Errorf("%s is not registered", want)
		}
	}
}

// The zone gauge is the leak detector, so a window that fails after setting the
// zone must read zero afterward.
func TestTheZoneGaugeReadsZeroAfterAWindowFails(t *testing.T) {
	h := windowAt(t, fourHostWorld(), stepEnteringMaintenance)
	h.pass()
	h.jobEnds(fdbCommandMaintenanceOn, true)
	h.pass()
	if got := testutil.ToFloat64(fdbMaintenanceZoneActive.WithLabelValues(testNamespace, drainedWorker)); got != 1 {
		t.Fatalf("zone gauge = %v after EnteringMaintenance, want 1", got)
	}

	h.atStep(stepMovingCoordinators, time.Now().Add(-time.Minute))
	ops := h.runUntilDone()
	if ops.Status.Phase != simplyblockv1alpha2.ControlPlaneOpsPhaseFailed {
		t.Fatalf("phase = %s, want Failed", ops.Status.Phase)
	}
	if got := testutil.ToFloat64(fdbMaintenanceZoneActive.WithLabelValues(testNamespace, drainedWorker)); got != 0 {
		t.Errorf("zone gauge = %v after the window failed, want 0", got)
	}
}
