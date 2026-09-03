// Capacity samples for logical volumes, devices, and storage nodes. The three
// share this file because they share a shape: the control plane exports the same
// five size gauges and a sample date under each prefix, so the only thing that
// differs is the prefix and the label naming the entity.

package prometheus

import (
	"context"
	"time"
)

// Capacity is one capacity sample for a logical volume, a device, or a storage
// node. Every size is in bytes.
//
// Provisioned against Used is the distinction the type exists for. A logical
// volume is thin-provisioned, so the size it was asked for and the space it has
// actually been given are different numbers, and only the first is visible
// anywhere in the Kubernetes API.
type Capacity struct {
	// Total is the space the entity can hold.
	Total int64
	// Used is the space it occupies after thin provisioning.
	Used int64
	// Free is what remains of Total.
	Free int64
	// Provisioned is the space promised out of it, which for an
	// over-provisioned pool may exceed Total.
	Provisioned int64
	// UtilizationPercent is the control plane's own rounding of Used over
	// Total, carried rather than recomputed so that it agrees with what the
	// control plane's own interfaces report.
	UtilizationPercent int32
	// SampledAt is when the control plane took the reading, which is not when
	// it was scraped and not when it was asked for. It is the zero time when
	// the control plane reports no date, which is how an entity that has never
	// been sampled is distinguished from one sampled at the epoch.
	SampledAt time.Time
}

// Sampled reports whether the control plane has ever taken this reading. An
// unsampled Capacity is all zeros, which is indistinguishable from a genuinely
// empty entity without asking.
func (c Capacity) Sampled() bool { return !c.SampledAt.IsZero() }

// The entity a capacity sample belongs to, as the exporter names it: the metric
// prefix, and the label carrying the entity's UUID.
const (
	volumeMetricPrefix = "lvol"
	volumeIDLabel      = "lvol"
	deviceMetricPrefix = "device"
	deviceIDLabel      = "device"
	nodeMetricPrefix   = "snode"
	nodeIDLabel        = "snode"
)

// VolumeCapacity returns the capacity sample for every logical volume in the
// cluster, keyed by volume UUID. A volume Prometheus has no sample for is
// absent from the result rather than present and zero.
//
// This is the only source with an answer. The control plane's own VolumeDTO
// carries a capacity block and reports zeros in it, while the same service
// exports these gauges with the real numbers.
func (p *Provider) VolumeCapacity(
	ctx context.Context,
	clusterUUID string,
) (map[string]Capacity, error) {
	return p.capacity(ctx, volumeMetricPrefix, volumeIDLabel, clusterUUID)
}

// DeviceCapacity returns the capacity sample for every device in the cluster,
// keyed by device UUID. A device Prometheus has no sample for is absent from
// the result rather than present and zero.
//
// The control plane's DeviceDTO does populate its capacity block, but its watch
// stream sends no event when the numbers move, so a mirror fed by that stream
// holds whatever the last snapshot said. This is the source that is current.
func (p *Provider) DeviceCapacity(
	ctx context.Context,
	clusterUUID string,
) (map[string]Capacity, error) {
	return p.capacity(ctx, deviceMetricPrefix, deviceIDLabel, clusterUUID)
}

// NodeCapacity returns the capacity sample for every storage node in the
// cluster, keyed by node UUID. A node Prometheus has no sample for is absent
// from the result rather than present and zero.
//
// A node's total is the sum of what its devices hold, so this is the same
// measurement as DeviceCapacity read one level up. It is exported separately
// because a caller asking how full a node is should not have to know which
// devices are in it.
func (p *Provider) NodeCapacity(
	ctx context.Context,
	clusterUUID string,
) (map[string]Capacity, error) {
	return p.capacity(ctx, nodeMetricPrefix, nodeIDLabel, clusterUUID)
}

// capacity assembles the samples for one entity kind. The metric names are
// derived from the prefix rather than listed per kind, because the exporter
// publishes the same set under both and a divergence between them would be a
// change in the control plane rather than a choice made here.
func (p *Provider) capacity(
	ctx context.Context,
	prefix, idLabel, clusterUUID string,
) (map[string]Capacity, error) {
	total := prefix + "_size_total"
	used := prefix + "_size_used"
	free := prefix + "_size_free"
	prov := prefix + "_size_prov"
	util := prefix + "_size_util"
	date := prefix + "_date"

	families, err := p.queryFamilyByLabel(
		ctx, []string{total, used, free, prov, util, date}, idLabel, clusterUUID,
	)
	if err != nil {
		return nil, err
	}

	out := make(map[string]Capacity, len(families))
	for id, series := range families {
		c := Capacity{
			Total:              whole(series[total]),
			Used:               whole(series[used]),
			Free:               whole(series[free]),
			Provisioned:        whole(series[prov]),
			UtilizationPercent: int32(whole(series[util])),
		}
		// A date of zero is the control plane saying it has no reading, not a
		// reading taken in 1970.
		if seconds := whole(series[date]); seconds > 0 {
			c.SampledAt = time.Unix(seconds, 0).UTC()
		}
		out[id] = c
	}
	return out, nil
}
