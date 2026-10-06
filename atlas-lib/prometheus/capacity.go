// Capacity samples for clusters, logical volumes, devices, storage nodes, and
// storage pools. The five share this file because they share a shape: the
// control plane's v2 exporter publishes the same size gauges under each prefix,
// so the only thing that differs is the prefix and the label naming the entity.
// It publishes no percentage and no sample date, so both are derived.

package prometheus

import (
	"context"
	"math"
	"time"
)

// Capacity is one capacity sample for a cluster, a logical volume, a device, a
// storage node, or a storage pool. Every size is in bytes.
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
	// over-provisioned pool may exceed Total. The exporter publishes it only for
	// clusters and storage nodes; it is zero for the other kinds.
	Provisioned int64
	// UtilizationPercent is Used over Total, rounded.
	UtilizationPercent int32
	// SampledAt is when this package read the sample; the exporter has no date
	// of its own. It is the zero time when the total is zero, which is how an
	// entity nothing has measured is told from an empty one.
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
	poolMetricPrefix   = "pool"
	poolIDLabel        = "pool"

	// A cluster is the one entity whose identifying label is the same one every
	// query already filters on, because every series in this exporter carries
	// the cluster it belongs to. So the cluster family groups by `cluster` and
	// yields exactly one entry.
	clusterMetricPrefix = "cluster"
	clusterIDLabel      = "cluster"
)

// ClusterCapacity returns the capacity sample for one cluster, and whether
// there is one. A cluster Prometheus has no sample for yields false rather than
// a zero sample, because zeros are the reading of an empty cluster.
//
// It differs from the other four in returning a single sample rather than a
// map. The cluster is the scope every one of those queries is already narrowed
// to, so grouping by the cluster label yields the one entry the caller asked
// for, and handing back a map of size one would make every caller unwrap it.
func (p *Provider) ClusterCapacity(
	ctx context.Context,
	clusterUUID string,
) (Capacity, bool, error) {
	samples, err := p.capacity(ctx, clusterMetricPrefix, clusterIDLabel, clusterUUID)
	if err != nil {
		return Capacity{}, false, err
	}
	sample, ok := samples[clusterUUID]
	return sample, ok, nil
}

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

// PoolCapacity returns the capacity sample for every storage pool in the
// cluster, keyed by pool UUID. A pool Prometheus has no sample for is absent
// from the result rather than present and zero.
//
// A pool's total is what its cluster has given it and its provisioned figure is
// the sum of what the volumes in it were promised, so the two answer the
// tenancy question the other families cannot: whether a pool is over-committed
// against the capacity limit it was carved out with.
func (p *Provider) PoolCapacity(
	ctx context.Context,
	clusterUUID string,
) (map[string]Capacity, error) {
	return p.capacity(ctx, poolMetricPrefix, poolIDLabel, clusterUUID)
}

// capacity assembles the samples for one entity kind. The metric names are
// derived from the prefix rather than listed per kind, because the exporter
// publishes the same set under both and a divergence between them would be a
// change in the control plane rather than a choice made here.
func (p *Provider) capacity(
	ctx context.Context,
	prefix, idLabel, clusterUUID string,
) (map[string]Capacity, error) {
	name := func(field string) string { return "simplyblock_" + prefix + "_size_" + field + "_bytes" }
	total, used, free, prov := name("total"), name("used"), name("free"), name("provisioned")

	families, err := p.queryFamilyByLabel(
		ctx, []string{total, used, free, prov}, idLabel, clusterUUID,
	)
	if err != nil {
		return nil, err
	}

	readAt := time.Now().UTC()
	out := make(map[string]Capacity, len(families))
	for id, series := range families {
		c := Capacity{
			Total:       whole(series[total]),
			Used:        whole(series[used]),
			Free:        whole(series[free]),
			Provisioned: whole(series[prov]),
		}
		if c.Total > 0 {
			c.UtilizationPercent = int32(math.Round(float64(c.Used) / float64(c.Total) * 100))
			c.SampledAt = readAt
		}
		out[id] = c
	}
	return out, nil
}
