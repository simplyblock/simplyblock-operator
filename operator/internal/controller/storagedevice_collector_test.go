// Tests for the device collector: the gauges it publishes from the device
// objects, the used size it reads from Prometheus, and the one event that needs a
// threshold to decide.
//
// The gauges are asserted through client_golang's testutil rather than by
// scraping, because what matters is the value under a label set and not the
// exposition format.

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// fakeCapacitySource is a static device-capacity provider, or a broken one when
// err is set.
type fakeCapacitySource struct {
	samples map[string]map[string]prometheus.Capacity // clusterUUID -> deviceID -> sample
	err     error
	calls   int
}

func (f *fakeCapacitySource) DeviceCapacity(
	_ context.Context, clusterUUID string,
) (map[string]prometheus.Capacity, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.samples[clusterUUID], nil
}

// collectorDevice is one mirror object as the mirror would have left it, in the
// phase given and with the size the hardware reported.
func collectorDevice(
	name string, phase simplyblockv1alpha2.StorageDevicePhase, deviceID string, total int64,
) *simplyblockv1alpha2.StorageDevice {
	return &simplyblockv1alpha2.StorageDevice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "sb", Name: name,
			Labels: map[string]string{
				simplyblockv1alpha2.DeviceLabelCluster: "production",
				simplyblockv1alpha2.DeviceLabelNode:    sdNodeCR,
				simplyblockv1alpha2.DeviceLabelWorker:  "worker-3",
			},
		},
		Spec: simplyblockv1alpha2.StorageDeviceSpec{NodeRef: sdNodeCR, DeviceID: deviceID},
		Status: simplyblockv1alpha2.StorageDeviceStatus{
			Phase:     phase,
			Capacity:  &simplyblockv1alpha2.DeviceCapacity{TotalBytes: ptr.To(total)},
			ClusterID: sdCluster,
			NodeID:    sdNodeID,
		},
	}
}

// collectorCluster is the StorageCluster whose warning threshold decides when a
// device is nearly full.
func collectorCluster(warnPercent int32) *simplyblockv1alpha1.StorageCluster {
	return &simplyblockv1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: "production"},
		Spec: simplyblockv1alpha1.StorageClusterSpec{
			WarningThresholdSpec: &simplyblockv1alpha1.CapacityThresholdSpec{
				Capacity: ptr.To(warnPercent),
			},
		},
	}
}

func newCollector(
	t *testing.T, capacity DeviceCapacitySource, objs ...client.Object,
) *StorageDeviceCollector {
	t.Helper()
	resetDeviceMetrics()
	scheme := newTestScheme(t, simplyblockv1alpha1.AddToScheme, simplyblockv1alpha2.AddToScheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &StorageDeviceCollector{
		Client:   c,
		Recorder: events.NewFakeRecorder(64),
		Capacity: capacity,
		Interval: time.Minute,
	}
}

func TestTheCollectorPublishesEachDevicesSizeAndPhase(t *testing.T) {
	c := newCollector(t, nil,
		collectorDevice("production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 4096),
		collectorDevice("production-7f3a9c-5e0000a2", simplyblockv1alpha2.StorageDevicePhaseFailed, "5e0000a2", 8192),
	)

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if got := testutil.ToFloat64(deviceCapacityBytes.WithLabelValues(
		"production", sdNodeCR, "production-7f3a9c-5e0000a1")); got != 4096 {
		t.Errorf("capacity gauge = %v, want 4096", got)
	}
	// The phase gauge is one series per phase, so a dashboard can graph a phase
	// without knowing which phases exist.
	if got := testutil.ToFloat64(devicePhaseState.WithLabelValues(
		"production", sdNodeCR, "production-7f3a9c-5e0000a1", "Online")); got != 1 {
		t.Errorf("Online phase gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(devicePhaseState.WithLabelValues(
		"production", sdNodeCR, "production-7f3a9c-5e0000a1", "Failed")); got != 0 {
		t.Errorf("a phase the device is not in = %v, want 0", got)
	}
}

// The count of failed devices per node is the redundancy signal: erasure coding
// survives a bounded number of simultaneous losses, and this is the input to
// whether the next one is survivable.
func TestTheCollectorCountsANodesDevicesAndItsFailedOnes(t *testing.T) {
	c := newCollector(t, nil,
		collectorDevice("production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 4096),
		collectorDevice("production-7f3a9c-5e0000a2", simplyblockv1alpha2.StorageDevicePhaseFailed, "5e0000a2", 4096),
		collectorDevice("production-7f3a9c-5e0000a3", simplyblockv1alpha2.StorageDevicePhaseDegraded, "5e0000a3", 4096),
	)

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if got := testutil.ToFloat64(nodeDeviceCount.WithLabelValues("production", sdNodeCR)); got != 3 {
		t.Errorf("device count = %v, want 3", got)
	}
	if got := testutil.ToFloat64(nodeDeviceFailedCount.WithLabelValues("production", sdNodeCR)); got != 1 {
		t.Errorf("failed count = %v, want 1", got)
	}
}

// A cluster at seventy per cent with one device at ninety-eight is a cluster
// about to have a problem its own thresholds cannot see, which is the metric
// this kind adds that nothing else can.
func TestTheCollectorPublishesUsedBytesFromPrometheus(t *testing.T) {
	capacity := &fakeCapacitySource{samples: map[string]map[string]prometheus.Capacity{
		sdCluster: {sdDevice: {Total: 4096, Used: 3000, SampledAt: time.Unix(1, 0)}},
	}}
	c := newCollector(t, capacity, collectorCluster(90),
		collectorDevice("production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 4096))

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if got := testutil.ToFloat64(deviceUsedBytes.WithLabelValues(
		"production", sdNodeCR, "production-7f3a9c-5e0000a1")); got != 3000 {
		t.Errorf("used gauge = %v, want 3000", got)
	}
	// One query per cluster, not one per device: eight hundred objects across a
	// hundred nodes is one query if the scope is the cluster.
	if capacity.calls != 1 {
		t.Errorf("%d capacity queries for one cluster, want 1", capacity.calls)
	}
}

// Without Prometheus there is no used size, and everything that does not depend
// on one is still published. The alternative is publishing zero, which is the
// reading of an empty drive rather than the absence of a reading.
func TestWithoutPrometheusTheRestIsStillPublished(t *testing.T) {
	c := newCollector(t, nil,
		collectorDevice("production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 4096))

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := testutil.CollectAndCount(deviceUsedBytes); got != 0 {
		t.Errorf("%d used-bytes series without a Prometheus, want none", got)
	}
	if got := testutil.CollectAndCount(deviceCapacityBytes); got != 1 {
		t.Errorf("%d capacity series, want 1", got)
	}
}

// A Prometheus that answers with an error must not cost the gauges that do not
// come from it.
func TestABrokenPrometheusDoesNotStopTheOtherGauges(t *testing.T) {
	c := newCollector(t, &fakeCapacitySource{err: errors.New("connection refused")},
		collectorDevice("production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 4096))

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("a failed capacity query must not fail the collection: %v", err)
	}
	if got := testutil.ToFloat64(deviceCapacityBytes.WithLabelValues(
		"production", sdNodeCR, "production-7f3a9c-5e0000a1")); got != 4096 {
		t.Errorf("capacity gauge = %v, want 4096", got)
	}
}

// The device that went away leaves no series behind. A gauge nobody updates any
// more keeps reporting its last value forever, which is a failed drive that a
// dashboard says is fine.
func TestTheCollectorDropsTheSeriesOfADeviceThatIsGone(t *testing.T) {
	device := collectorDevice(
		"production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 4096)
	c := newCollector(t, nil, device)

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := c.Delete(context.Background(), device); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if got := testutil.CollectAndCount(deviceCapacityBytes); got != 0 {
		t.Errorf("%d capacity series after the device went away, want none", got)
	}
	if got := testutil.CollectAndCount(nodeDeviceCount); got != 0 {
		t.Errorf("%d node series after its last device went away, want none", got)
	}
}

// The threshold is the cluster's own, so a device is nearly full by the standard
// its cluster declares rather than by one this collector invents.
func TestADeviceOverItsClustersThresholdIsWarnedAboutOnce(t *testing.T) {
	capacity := &fakeCapacitySource{samples: map[string]map[string]prometheus.Capacity{
		sdCluster: {sdDevice: {Total: 1000, Used: 800, SampledAt: time.Unix(1, 0)}},
	}}
	c := newCollector(t, capacity, collectorCluster(75),
		collectorDevice("production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 1000))

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	rec := recorderOf(t, c)
	if !announced(rec, "DeviceNearlyFull") {
		t.Fatal("a device over its cluster's threshold was not warned about")
	}

	// Still full on the next pass, and still the same fact: warning about it
	// every tick would make the event stream a poll log.
	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if announced(rec, "DeviceNearlyFull") {
		t.Error("the same device was warned about twice for one crossing")
	}

	// It empties, then fills again, which is a second crossing and a second
	// warning.
	capacity.samples[sdCluster][sdDevice] = prometheus.Capacity{
		Total: 1000, Used: 100, SampledAt: time.Unix(2, 0),
	}
	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	capacity.samples[sdCluster][sdDevice] = prometheus.Capacity{
		Total: 1000, Used: 900, SampledAt: time.Unix(3, 0),
	}
	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !announced(rec, "DeviceNearlyFull") {
		t.Error("a device that filled up again was not warned about")
	}
}

func TestADeviceUnderTheThresholdIsNotWarnedAbout(t *testing.T) {
	capacity := &fakeCapacitySource{samples: map[string]map[string]prometheus.Capacity{
		sdCluster: {sdDevice: {Total: 1000, Used: 500, SampledAt: time.Unix(1, 0)}},
	}}
	c := newCollector(t, capacity, collectorCluster(75),
		collectorDevice("production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 1000))

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if announced(recorderOf(t, c), "DeviceNearlyFull") {
		t.Error("a half-full device was warned about")
	}
}

// A cluster that declares no threshold still gets a warning, at the default,
// because a full device is worth saying whether or not anybody configured a
// number.
func TestAClusterWithNoThresholdFallsBackToTheDefault(t *testing.T) {
	capacity := &fakeCapacitySource{samples: map[string]map[string]prometheus.Capacity{
		sdCluster: {sdDevice: {Total: 1000, Used: 990, SampledAt: time.Unix(1, 0)}},
	}}
	c := newCollector(t, capacity,
		&simplyblockv1alpha1.StorageCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "sb", Name: "production"},
		},
		collectorDevice("production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 1000))

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !announced(recorderOf(t, c), "DeviceNearlyFull") {
		t.Error("no threshold configured must not mean no warning")
	}
}

func recorderOf(t *testing.T, c *StorageDeviceCollector) *events.FakeRecorder {
	t.Helper()
	rec, ok := c.Recorder.(*events.FakeRecorder)
	if !ok {
		t.Fatalf("recorder is %T, not a FakeRecorder", c.Recorder)
	}
	return rec
}
