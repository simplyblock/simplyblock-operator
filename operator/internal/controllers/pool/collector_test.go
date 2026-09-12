// Tests for the pool collector: what one pass publishes, and what it stops
// publishing.
//
// The second half is the one worth having. A gauge nobody updates keeps its last
// value forever, so a pool that went away would be reported half full until the
// process restarted, and a dashboard that says a deleted pool is fine is worse
// than no dashboard. One pass therefore rebuilds every series from the pools
// that exist right now, and that is what the second test pins.

package pool

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	atlasprom "github.com/simplyblock/atlas/prometheus"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// fakeCapacity answers from a table and counts its calls, because one pass must
// query a cluster once rather than once per pool.
type fakeCapacity struct {
	byCluster map[string]map[string]atlasprom.Capacity
	calls     int
}

func (f *fakeCapacity) PoolCapacity(
	_ context.Context, clusterUUID string,
) (map[string]atlasprom.Capacity, error) {
	f.calls++
	return f.byCluster[clusterUUID], nil
}

// fakeVolumes answers the logical-volume count from a table.
type fakeVolumes map[string]int

func (f fakeVolumes) PoolVolumeCounts() map[string]int { return f }

// gaugeValue reads one labeled series.
//
// A series a pass did not publish reads as zero here rather than as absent,
// because GetMetricWithLabelValues creates the one it is asked for. That is
// enough for what these tests assert: each compares a pass that published a
// number against a pass that did not, and zero is the answer either way when the
// series is gone.
func gaugeValue(t *testing.T, vec *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	gauge, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("read the gauge: %v", err)
	}
	var metric dto.Metric
	if err := gauge.Write(&metric); err != nil {
		t.Fatalf("write the gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}

func collectOnce(t *testing.T, c *Collector) {
	t.Helper()
	if err := c.collect(context.Background(), logr.Discard()); err != nil {
		t.Fatalf("collect: %v", err)
	}
}

// One pass publishes what a pool is, what it holds, and how many classes draw
// from it.
func TestOnePassPublishesThePoolsGauges(t *testing.T) {
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.Phase = simplyblockv1alpha2.StoragePoolPhaseReady
		p.Status.StorageClassNames = []string{"fast-xfs", "archive-ext4"}
		p.Spec.Limits = &simplyblockv1alpha2.PoolLimits{Capacity: "10G"}
	})
	capacity := &fakeCapacity{byCluster: map[string]map[string]atlasprom.Capacity{
		testClusterUUID: {testPoolUUID: {Used: 4096, Provisioned: 8192}},
	}}
	c := &Collector{
		Client:   newClient(t, newCluster(testClusterUUID), ready),
		Capacity: capacity,
		Volumes:  fakeVolumes{testPoolUUID: 7},
	}

	collectOnce(t, c)

	if got := gaugeValue(t, poolUsedBytes, testCluster, "tenant-a"); got != 4096 {
		t.Errorf("used_bytes = %v, want 4096", got)
	}
	if got := gaugeValue(t, poolProvisionedBytes, testCluster, "tenant-a"); got != 8192 {
		t.Errorf("provisioned_bytes = %v, want 8192", got)
	}
	if got := gaugeValue(t, poolStorageClassesCount, testCluster, "tenant-a"); got != 2 {
		t.Errorf("storageclasses_count = %v, want 2", got)
	}
	if got := gaugeValue(t, poolVolumesCount, testCluster, "tenant-a"); got != 7 {
		t.Errorf("volumes_count = %v, want 7", got)
	}
	if got := gaugeValue(t, poolPhaseState, testCluster, "tenant-a", "Ready"); got != 1 {
		t.Errorf("phase_state{phase=Ready} = %v, want 1", got)
	}
	if got := gaugeValue(t, poolPhaseState, testCluster, "tenant-a", "Deleting"); got != 0 {
		t.Errorf("phase_state{phase=Deleting} = %v, want 0", got)
	}
}

// A pool that goes away stops being reported. The pass rebuilds every series
// from the pools that exist, so the deleted pool's used-bytes reading does not
// survive as a number a dashboard still draws.
func TestAPoolThatGoesAwayStopsBeingReported(t *testing.T) {
	ready := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Status.Phase = simplyblockv1alpha2.StoragePoolPhaseReady
		// No finalizer, so the delete below actually removes the object rather
		// than parking it in Terminating — which is a state the collector
		// deliberately still reports, and not what this test is about.
		p.Finalizers = nil
	})
	capacity := &fakeCapacity{byCluster: map[string]map[string]atlasprom.Capacity{
		testClusterUUID: {testPoolUUID: {Used: 4096}},
	}}
	client := newClient(t, newCluster(testClusterUUID), ready)
	c := &Collector{Client: client, Capacity: capacity}

	collectOnce(t, c)
	if got := gaugeValue(t, poolUsedBytes, testCluster, "tenant-a"); got != 4096 {
		t.Fatalf("used_bytes = %v before the pool went away, want 4096", got)
	}

	if err := client.Delete(context.Background(), ready); err != nil {
		t.Fatalf("delete the pool: %v", err)
	}
	collectOnce(t, c)

	if got := gaugeValue(t, poolUsedBytes, testCluster, "tenant-a"); got != 0 {
		t.Errorf("used_bytes = %v after the pool went away, want the series rebuilt without it", got)
	}
}

// A pool over its capacity limit is announced once per crossing rather than once
// per pass, and a pool that drops back under is forgotten so the next filling is
// a second warning.
func TestCapacityExhaustedFiresOncePerCrossing(t *testing.T) {
	full := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
		p.Spec.Limits = &simplyblockv1alpha2.PoolLimits{Capacity: "1K"}
	})
	capacity := &fakeCapacity{byCluster: map[string]map[string]atlasprom.Capacity{
		testClusterUUID: {testPoolUUID: {Provisioned: 1 << 20}},
	}}
	rec := &recorder{}
	c := &Collector{
		Client:   newClient(t, newCluster(testClusterUUID), full),
		Recorder: rec,
		Capacity: capacity,
	}

	collectOnce(t, c)
	collectOnce(t, c)

	if got := rec.count(CapacityExhausted); got != 1 {
		t.Errorf("%s was emitted %d times over two passes, want 1", CapacityExhausted, got)
	}

	// The pool drains, then fills again: that is a second crossing.
	capacity.byCluster[testClusterUUID][testPoolUUID] = atlasprom.Capacity{Provisioned: 1}
	collectOnce(t, c)
	capacity.byCluster[testClusterUUID][testPoolUUID] = atlasprom.Capacity{Provisioned: 1 << 20}
	collectOnce(t, c)

	if got := rec.count(CapacityExhausted); got != 2 {
		t.Errorf("%s was emitted %d times across two crossings, want 2", CapacityExhausted, got)
	}
}

// A pool with no capacity limit cannot be exhausted, so it is never announced.
// Comparing against a limit of zero would warn about every pool that holds
// anything at all.
func TestAnUnlimitedPoolIsNeverExhausted(t *testing.T) {
	unlimited := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	capacity := &fakeCapacity{byCluster: map[string]map[string]atlasprom.Capacity{
		testClusterUUID: {testPoolUUID: {Provisioned: 1 << 40}},
	}}
	rec := &recorder{}
	c := &Collector{
		Client:   newClient(t, newCluster(testClusterUUID), unlimited),
		Recorder: rec,
		Capacity: capacity,
	}

	collectOnce(t, c)

	if rec.has(CapacityExhausted) {
		t.Errorf("a pool with no capacity limit was announced as exhausted: %+v", rec.events)
	}
}

// One pass queries each cluster once rather than once per pool, because each
// query is an HTTP round trip.
func TestOnePassQueriesEachClusterOnce(t *testing.T) {
	first := newPool("tenant-a", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = testPoolUUID
	})
	second := newPool("tenant-b", func(p *simplyblockv1alpha2.StoragePool) {
		p.Status.UUID = "another-pool"
	})
	capacity := &fakeCapacity{byCluster: map[string]map[string]atlasprom.Capacity{
		testClusterUUID: {testPoolUUID: {Used: 1}, "another-pool": {Used: 2}},
	}}
	c := &Collector{
		Client:   newClient(t, newCluster(testClusterUUID), first, second),
		Capacity: capacity,
	}

	collectOnce(t, c)

	if capacity.calls != 1 {
		t.Errorf("the metrics endpoint was queried %d times for one cluster, want 1", capacity.calls)
	}
}
