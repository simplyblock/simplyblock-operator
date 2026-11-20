// Per-device I/O rates and latency, which a slow-device assessment compares
// across a cluster's fleet. It is separate from the volume I/O family because a
// device's latency is part of the reading: the control plane exports it as
// cumulative SPDK ticks beside the operation count, so a mean latency is the
// rate of one divided by the rate of the other, and a duration needs the tick
// rate the same exporter publishes per device.
package prometheus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// DeviceIO is one device's load and latency over a window, as rates per
// second. Reads and writes are kept apart because they are what the counters
// are, and a caller weighing a device's load against its fleet adds them.
type DeviceIO struct {
	ReadOpsPerSec    float64
	WriteOpsPerSec   float64
	ReadBytesPerSec  float64
	WriteBytesPerSec float64

	// ReadLatencyTicksPerSec and WriteLatencyTicksPerSec are the rates of the
	// cumulative latency counters: ticks of latency accrued per second of wall
	// time. Divided by the operation rate they are ticks per operation.
	ReadLatencyTicksPerSec  float64
	WriteLatencyTicksPerSec float64

	// TickRateHz is how many SPDK ticks make a second on the device's node. It
	// is zero when the control plane does not export it, which an older one
	// does not, and then no latency in seconds is claimed.
	TickRateHz float64
}

// IOPS is read plus write operations per second.
func (d DeviceIO) IOPS() float64 { return d.ReadOpsPerSec + d.WriteOpsPerSec }

// ThroughputBytesPerSec is read plus write bytes per second.
func (d DeviceIO) ThroughputBytesPerSec() float64 { return d.ReadBytesPerSec + d.WriteBytesPerSec }

// MeanLatencyTicks is the mean latency of one operation in SPDK ticks, and
// false for a device that served no operation in the window: an idle device
// has no latency, and a zero would read as the fastest device of the fleet.
func (d DeviceIO) MeanLatencyTicks() (float64, bool) {
	ops := d.IOPS()
	if ops <= 0 {
		return 0, false
	}
	return (d.ReadLatencyTicksPerSec + d.WriteLatencyTicksPerSec) / ops, true
}

// MeanLatencySeconds is the mean latency of one operation in seconds, and false
// when the device was idle or its tick rate is not known.
func (d DeviceIO) MeanLatencySeconds() (float64, bool) {
	ticks, ok := d.MeanLatencyTicks()
	if !ok || d.TickRateHz <= 0 {
		return 0, false
	}
	return ticks / d.TickRateHz, true
}

// The counters the device I/O query rates, by the kind label each series is
// given. rate() drops the metric name, so the label is what tells the series of
// one query apart.
var deviceIOCounters = []string{
	"read_operations", "write_operations",
	"read_bytes", "write_bytes",
	"read_latency_ticks", "write_latency_ticks",
}

// deviceTickRateKind labels the tick-rate gauge in the same query.
const deviceTickRateKind = "tick_rate"

// DeviceIO returns the I/O rates of every device of the cluster over the given
// window, keyed by device UUID. A device absent from Prometheus is absent from
// the result.
//
// Every figure comes back in one query so that they describe the same instant:
// a latency rate from one scrape divided by an operation rate from the next
// would be a latency of nothing in particular. The window has to hold at least
// two of the control plane's samples, which it collects every fifteen seconds.
func (p *Provider) DeviceIO(
	ctx context.Context,
	clusterUUID string,
	window time.Duration,
) (map[string]DeviceIO, error) {
	vec, err := p.queryVector(ctx, deviceIOQuery(clusterUUID, window))
	if err != nil {
		return nil, err
	}
	out := make(map[string]DeviceIO)
	for _, sample := range vec {
		id := string(sample.Metric[model.LabelName(deviceIDLabel)])
		if id == "" {
			continue
		}
		io := out[id]
		v := float64(sample.Value)
		switch sample.Metric["kind"] {
		case "read_operations":
			io.ReadOpsPerSec = v
		case "write_operations":
			io.WriteOpsPerSec = v
		case "read_bytes":
			io.ReadBytesPerSec = v
		case "write_bytes":
			io.WriteBytesPerSec = v
		case "read_latency_ticks":
			io.ReadLatencyTicksPerSec = v
		case "write_latency_ticks":
			io.WriteLatencyTicksPerSec = v
		case deviceTickRateKind:
			io.TickRateHz = v
		}
		out[id] = io
	}
	return out, nil
}

// deviceIOQuery is the rate of every device counter and the tick-rate gauge,
// each grouped by device and labeled with its kind, joined into one vector.
func deviceIOQuery(clusterUUID string, window time.Duration) string {
	seconds := int64(window / time.Second)
	parts := make([]string, 0, len(deviceIOCounters)+1)
	for _, kind := range deviceIOCounters {
		parts = append(parts, fmt.Sprintf(
			`label_replace(sum by (%s) (rate(simplyblock_device_%s_total{cluster=%q}[%ds])), "kind", %q, "", "")`,
			deviceIDLabel, kind, clusterUUID, seconds, kind))
	}
	parts = append(parts, fmt.Sprintf(
		`label_replace(max by (%s) (simplyblock_device_tick_rate_hertz{cluster=%q}), "kind", %q, "", "")`,
		deviceIDLabel, clusterUUID, deviceTickRateKind))
	return strings.Join(parts, " or ")
}
