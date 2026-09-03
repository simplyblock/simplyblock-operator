// Per-volume IOPS and throughput, which the rebalancer ranks volumes by. It is
// separate from the capacity family because the control plane exports these as
// four already-per-second gauges that have to be added together rather than as
// one sample per entity.

package prometheus

import "context"

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
// The four gauges are fetched in one query rather than four, so that the
// numbers summed into one result were sampled at one instant.
func (p *Provider) VolumeIO(
	ctx context.Context,
	clusterUUID string,
) (map[string]VolumeIO, error) {
	const (
		readIOPS   = "lvol_read_io_ps"
		writeIOPS  = "lvol_write_io_ps"
		readBytes  = "lvol_read_bytes_ps"
		writeBytes = "lvol_write_bytes_ps"
	)

	families, err := p.queryFamilyByLabel(
		ctx,
		[]string{readIOPS, writeIOPS, readBytes, writeBytes},
		volumeIDLabel,
		clusterUUID,
	)
	if err != nil {
		return nil, err
	}

	out := make(map[string]VolumeIO, len(families))
	for id, series := range families {
		out[id] = VolumeIO{
			IOPS:                  series[readIOPS] + series[writeIOPS],
			ThroughputBytesPerSec: series[readBytes] + series[writeBytes],
		}
	}
	return out, nil
}
