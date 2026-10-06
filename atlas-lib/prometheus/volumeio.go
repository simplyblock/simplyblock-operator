// Per-volume IOPS and throughput, which the rebalancer ranks volumes by. It is
// separate from the capacity family because the control plane exports these as
// cumulative counters that have to be turned into rates and added together
// rather than read as one sample per entity.

package prometheus

import (
	"context"
	"fmt"

	"github.com/prometheus/common/model"
)

// ioRateWindow is how far back the counters are differenced. It has to hold at
// least two scrapes, and it smooths a volume's bursts into the load a placement
// decision should see.
const ioRateWindow = "5m"

// VolumeIO is the combined read and write load on one logical volume, as the
// control plane last measured it. Reads and writes are summed because a
// placement decision is about how busy a volume is, not about which direction
// it is busy in.
type VolumeIO struct {
	// IOPS is read plus write operations per second.
	IOPS float64
	// ThroughputBytesPerSec is read plus write bytes per second.
	ThroughputBytesPerSec float64
}

// VolumeIO returns the load on every logical volume in the cluster, keyed by
// volume UUID. A volume absent from Prometheus is absent from the result.
//
// The v2 exporter publishes cumulative counters, so each figure is the rate of
// the read counter plus the rate of the write counter. Both figures come back in
// one query so that they describe the same instant. rate() drops the metric
// name, which is why each is labeled kind="ops" or kind="bytes" to tell them apart.
func (p *Provider) VolumeIO(
	ctx context.Context,
	clusterUUID string,
) (map[string]VolumeIO, error) {
	query := fmt.Sprintf(`label_replace(%s, "kind", "ops", "", "") or label_replace(%s, "kind", "bytes", "", "")`,
		readPlusWriteRate("operations", clusterUUID), readPlusWriteRate("bytes", clusterUUID))

	vec, err := p.queryVector(ctx, query)
	if err != nil {
		return nil, err
	}

	out := make(map[string]VolumeIO)
	for _, sample := range vec {
		id := string(sample.Metric[model.LabelName(volumeIDLabel)])
		if id == "" {
			continue
		}
		io := out[id]
		switch sample.Metric["kind"] {
		case "ops":
			io.IOPS = float64(sample.Value)
		case "bytes":
			io.ThroughputBytesPerSec = float64(sample.Value)
		}
		out[id] = io
	}
	return out, nil
}

// readPlusWriteRate is the per-volume rate of a read counter plus the matching
// write counter. Each side is aggregated on its own because rate() over one
// selector matching both names yields two series with the same labels, which
// Prometheus rejects.
func readPlusWriteRate(unit, clusterUUID string) string {
	side := func(direction string) string {
		return fmt.Sprintf(`sum by (%s) (rate(simplyblock_lvol_%s_%s_total{cluster=%q}[%s]))`,
			volumeIDLabel, direction, unit, clusterUUID, ioRateWindow)
	}
	return side("read") + " + " + side("write")
}
