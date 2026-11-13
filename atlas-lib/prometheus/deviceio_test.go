// Tests for the per-device I/O family: which query is issued, and how the series
// it returns are folded into one reading per device.

package prometheus

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
)

// kindSample is one series of the device I/O query: the rate() it carries has
// lost its metric name, so the kind label is the only thing naming it.
func kindSample(device, kind string, value float64) *model.Sample {
	return &model.Sample{
		Metric: model.Metric{"device": model.LabelValue(device), "kind": model.LabelValue(kind)},
		Value:  model.SampleValue(value),
	}
}

func TestDeviceIOFoldsEveryKindIntoOneReading(t *testing.T) {
	api := &stubAPI{vector: model.Vector{
		kindSample(testDevice, "read_operations", 300),
		kindSample(testDevice, "write_operations", 700),
		kindSample(testDevice, "read_bytes", 300*4096),
		kindSample(testDevice, "write_bytes", 700*65536),
		kindSample(testDevice, "read_latency_ticks", 300*0.0002*2.0e9),
		kindSample(testDevice, "write_latency_ticks", 700*0.0012*2.0e9),
		kindSample(testDevice, "tick_rate", 2.0e9),
	}}

	got, err := NewWithAPI(api).DeviceIO(context.Background(), testCluster, 10*time.Minute)
	if err != nil {
		t.Fatalf("DeviceIO: %v", err)
	}
	io, ok := got[testDevice]
	if !ok {
		t.Fatalf("DeviceIO returned %v, want a reading for %s", got, testDevice)
	}
	if io.IOPS() != 1000 {
		t.Errorf("IOPS = %v, want 1000", io.IOPS())
	}
	if io.ThroughputBytesPerSec() != 300*4096+700*65536 {
		t.Errorf("throughput = %v", io.ThroughputBytesPerSec())
	}
	// (300 x 0.2 ms + 700 x 1.2 ms) / 1000 = 0.9 ms per I/O.
	latency, ok := io.MeanLatencySeconds()
	if !ok || math.Abs(latency-0.0009) > 1e-9 {
		t.Errorf("MeanLatencySeconds = %v, %v, want 0.0009, true", latency, ok)
	}
}

// Latency in seconds needs the tick rate, which an older control plane does not
// export. The reading is still returned, because the latency in ticks can be
// compared between devices of one CPU type, but it does not claim a duration.
func TestDeviceIOWithoutATickRateHasNoLatencyInSeconds(t *testing.T) {
	api := &stubAPI{vector: model.Vector{
		kindSample(testDevice, "write_operations", 100),
		kindSample(testDevice, "write_latency_ticks", 100*5000),
	}}

	got, err := NewWithAPI(api).DeviceIO(context.Background(), testCluster, 10*time.Minute)
	if err != nil {
		t.Fatalf("DeviceIO: %v", err)
	}
	io := got[testDevice]
	if _, ok := io.MeanLatencySeconds(); ok {
		t.Error("a reading with no tick rate reported a latency in seconds")
	}
	if ticks, ok := io.MeanLatencyTicks(); !ok || ticks != 5000 {
		t.Errorf("MeanLatencyTicks = %v, %v, want 5000, true", ticks, ok)
	}
}

// An idle device has no mean latency: zero operations divide into nothing, and
// a latency of zero would read as the fastest device in the fleet.
func TestAnIdleDeviceHasNoMeanLatency(t *testing.T) {
	io := DeviceIO{TickRateHz: 2.0e9}
	if _, ok := io.MeanLatencySeconds(); ok {
		t.Error("an idle device reported a mean latency")
	}
}

func TestDeviceIOQueryRatesTheV2CountersOverTheWindowAndPinsTheCluster(t *testing.T) {
	api := &stubAPI{vector: model.Vector{}}
	if _, err := NewWithAPI(api).DeviceIO(context.Background(), testCluster, 7*time.Minute); err != nil {
		t.Fatalf("DeviceIO: %v", err)
	}
	if len(api.queries) != 1 {
		t.Fatalf("issued %d queries, want one: every figure must describe the same instant", len(api.queries))
	}
	q := api.queries[0]
	for _, want := range []string{
		"rate(simplyblock_device_read_operations_total{cluster=\"" + testCluster + "\"}[420s])",
		"simplyblock_device_write_operations_total",
		"simplyblock_device_read_bytes_total",
		"simplyblock_device_write_bytes_total",
		"simplyblock_device_read_latency_ticks_total",
		"simplyblock_device_write_latency_ticks_total",
		"simplyblock_device_tick_rate_hertz{cluster=\"" + testCluster + "\"}",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q does not contain %q", q, want)
		}
	}
}

func TestADeviceIOSeriesWithNoDeviceLabelIsDropped(t *testing.T) {
	api := &stubAPI{vector: model.Vector{kindSample("", "write_operations", 10)}}
	got, err := NewWithAPI(api).DeviceIO(context.Background(), testCluster, time.Minute)
	if err != nil {
		t.Fatalf("DeviceIO: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DeviceIO = %v, want nothing for a series naming no device", got)
	}
}
