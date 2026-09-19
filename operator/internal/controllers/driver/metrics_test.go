// What the driver's gauges say, and what they deliberately do not say yet.

package driver

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// aDriverIn builds a reconciler over a driver in its own namespace, so one
// test's gauges are not another's: the metrics are package-level and the
// namespace is their label.
func aDriverIn(t *testing.T, namespace string) (
	*SimplyblockDriverReconciler, *simplyblockv1alpha2.SimplyblockDriver,
) {
	t.Helper()
	scheme := reconcilerScheme(t)
	d := testDriver("simplyblock")
	d.Namespace = namespace
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(d).WithStatusSubresource(d).Build()
	return &SimplyblockDriverReconciler{Client: c, Scheme: scheme}, d
}

// The ready count and the expected count are one signal in two series, and the
// design says neither half means anything alone. A pass that published one and
// not the other would give a dashboard a ratio it cannot compute.
func TestTheNodePluginCountsArePublishedTogether(t *testing.T) {
	r, d := aDriverIn(t, "counts")

	err := r.setHealth(context.Background(), d, health{
		phase:           simplyblockv1alpha2.SimplyblockDriverPhaseDegraded,
		nodesReady:      2,
		nodesTotal:      3,
		controllerReady: true,
		message:         "one node plugin is not ready",
	})
	if err != nil {
		t.Fatalf("setHealth: %v", err)
	}

	if got := testutil.ToFloat64(driverNodesReady.WithLabelValues("counts")); got != 2 {
		t.Errorf("nodes_ready_count is %v, want 2", got)
	}
	if got := testutil.ToFloat64(driverNodesExpected.WithLabelValues("counts")); got != 3 {
		t.Errorf("nodes_expected_count is %v, want 3", got)
	}
}

// Provisioning stops when the controller plugin is not serving, so the gauge is
// the one that turns that into an alert rather than a phase somebody reads.
func TestTheControllerStateIsOneOrZero(t *testing.T) {
	r, d := aDriverIn(t, "controller")

	serving := health{phase: simplyblockv1alpha2.SimplyblockDriverPhaseReady,
		nodesReady: 3, nodesTotal: 3, controllerReady: true}
	if err := r.setHealth(context.Background(), d, serving); err != nil {
		t.Fatalf("setHealth: %v", err)
	}
	if got := testutil.ToFloat64(driverControllerReady.WithLabelValues("controller")); got != 1 {
		t.Errorf("controller_ready_state is %v while it serves, want 1", got)
	}

	down := health{phase: simplyblockv1alpha2.SimplyblockDriverPhaseUnavailable,
		nodesReady: 3, nodesTotal: 3}
	if err := r.setHealth(context.Background(), d, down); err != nil {
		t.Fatalf("setHealth: %v", err)
	}
	if got := testutil.ToFloat64(driverControllerReady.WithLabelValues("controller")); got != 0 {
		t.Errorf("controller_ready_state is %v while it is down, want 0", got)
	}
}

// status.version is not written by anything yet, and the gauge says so rather
// than claiming a version. An _info gauge is 1 for the fact it carries in its
// labels, so 0 is the honest reading for a fact nobody has established.
func TestTheVersionGaugeIsZeroUntilSomethingReportsAVersion(t *testing.T) {
	r, d := aDriverIn(t, "version")

	err := r.setHealth(context.Background(), d, health{
		phase: simplyblockv1alpha2.SimplyblockDriverPhaseReady, nodesReady: 1, nodesTotal: 1,
		controllerReady: true,
	})
	if err != nil {
		t.Fatalf("setHealth: %v", err)
	}
	if got := testutil.ToFloat64(driverVersionInfo.WithLabelValues("version", "")); got != 0 {
		t.Errorf("version_info is %v with no reported version, want 0", got)
	}

	// And it is 1 the moment one arrives, so the series is right on the day §5
	// lands rather than needing a second change. The version is written through
	// the client because that is where setHealth reads it back from.
	var stored simplyblockv1alpha2.SimplyblockDriver
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(d), &stored); err != nil {
		t.Fatalf("read the driver back: %v", err)
	}
	stored.Status.Version = "v26.2.6"
	if err := r.Status().Update(context.Background(), &stored); err != nil {
		t.Fatalf("record a reported version: %v", err)
	}
	if err := r.setHealth(context.Background(), d, health{
		phase: simplyblockv1alpha2.SimplyblockDriverPhaseReady, nodesReady: 1, nodesTotal: 1,
		controllerReady: true,
	}); err != nil {
		t.Fatalf("setHealth: %v", err)
	}
	if got := testutil.ToFloat64(driverVersionInfo.WithLabelValues("version", "v26.2.6")); got != 1 {
		t.Errorf("version_info is %v for the reported version, want 1", got)
	}
	versions := versionSeriesIn(driverVersionInfo, "version")
	if len(versions) != 1 || versions[0] != "v26.2.6" {
		t.Errorf("the gauge holds %v for this namespace; a version that was "+
			"replaced leaves a second series claiming to be the current one",
			versions)
	}
}

// versionSeriesIn is the version labels the gauge holds for one namespace. The
// point of the reading is how many there are: a metric whose label carries the
// fact has to drop the old fact when it changes, and nothing but the series
// themselves shows whether it did.
func versionSeriesIn(collector prometheus.Collector, namespace string) []string {
	gathered := make(chan prometheus.Metric, 32)
	collector.Collect(gathered)
	close(gathered)

	var versions []string
	for metric := range gathered {
		var written dto.Metric
		if err := metric.Write(&written); err != nil {
			continue
		}
		labels := map[string]string{}
		for _, pair := range written.GetLabel() {
			labels[pair.GetName()] = pair.GetValue()
		}
		if labels["namespace"] == namespace {
			versions = append(versions, labels["version"])
		}
	}
	return versions
}
