// The device collector: what the operator publishes about devices on a timer
// rather than on an event.
//
// It is separate from the mirror next door because the two answer to different
// clocks. The mirror writes a Kubernetes object when the control plane says
// something changed, and everything it publishes is a property of the device
// that does not move: its identity, its phase, and its size. What a device holds
// moves continuously, arrives from Prometheus rather than from the device
// stream, and is worth neither an etcd write nor a reconcile, so it is read on a
// tick, published as a gauge, and never stored.
//
// One pass rebuilds every series from the full list of objects. That is
// deliberately not incremental: a collector that updated one device's gauges as
// its object changed would have to remember which series to delete when an
// object went away, and a series nobody deletes reports a failed drive as
// healthy forever.

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/simplyblock/atlas/prometheus"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// DefaultDeviceCollectInterval is how often the gauges are rebuilt. It is the
// resolution of a capacity graph rather than of an alert: a device fills over
// hours, and the phase it is in is published by the mirror's own writes as well.
const DefaultDeviceCollectInterval = 30 * time.Second

// DefaultNearlyFullPercent is the occupancy a device is warned about at when its
// cluster declares no warning threshold of its own. A full device is worth
// saying whether or not anybody configured a number.
const DefaultNearlyFullPercent = 80

// DeviceCapacitySource supplies what a device holds. It is satisfied by
// atlas-lib's prometheus.Provider, and it is an interface here so that a test
// needs no Prometheus and a deployment without one can pass nil.
//
// The control plane is not the source. Its DeviceDTO carries a capacity block
// whose numbers its watch stream never updates, while the metrics the same
// service exports carry the current ones.
type DeviceCapacitySource interface {
	// DeviceCapacity returns the sample for every device of a cluster, keyed by
	// control-plane device id. A device with no sample is absent.
	DeviceCapacity(ctx context.Context, clusterUUID string) (map[string]prometheus.Capacity, error)
}

// StorageDeviceCollector publishes the device gauges and the one device event
// that needs a measurement to decide.
type StorageDeviceCollector struct {
	client.Client

	// Recorder announces a device crossing its cluster's capacity threshold.
	Recorder events.EventRecorder

	// Capacity is where the used size comes from. A nil source is a deployment
	// with no reachable Prometheus: every gauge that does not depend on one is
	// still published, and the used size is absent rather than zero, because a
	// zero is the reading of an empty drive and not the absence of a reading.
	Capacity DeviceCapacitySource

	// Interval is how often to collect. Zero selects
	// DefaultDeviceCollectInterval.
	Interval time.Duration

	// full remembers which devices have already been warned about, so a device
	// that stays full is one event rather than one per tick. A device that
	// empties is forgotten, which is what makes the next filling a second
	// crossing and a second warning.
	full map[string]bool
}

// +kubebuilder:rbac:groups=storage.simplyblock.io,resources=storageclusters,verbs=get;list;watch

// NeedLeaderElection implements manager.LeaderElectionRunnable. The gauges are
// per-cluster facts rather than per-replica ones, and the events are written to
// the API: a follower publishing both would double every event and make the
// gauges depend on which replica a scrape reached.
func (c *StorageDeviceCollector) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: collect once immediately, so a restart does
// not leave the gauges empty for an interval, then on every tick.
func (c *StorageDeviceCollector) Start(ctx context.Context) error {
	interval := c.Interval
	if interval == 0 {
		interval = DefaultDeviceCollectInterval
	}
	log := logf.FromContext(ctx).WithName("storagedevice-collector")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.collect(ctx); err != nil {
			// A failed pass is not a failed operator: the next tick tries again,
			// and the gauges keep whatever the last successful pass left.
			log.Error(err, "collecting storage device metrics")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// collect rebuilds every series from the current objects and emits the events
// the readings justify.
func (c *StorageDeviceCollector) collect(ctx context.Context) error {
	var devices simplyblockv1alpha2.StorageDeviceList
	if err := c.List(ctx, &devices); err != nil {
		return fmt.Errorf("list storage devices: %w", err)
	}

	resetDeviceMetrics()

	type nodeKey struct{ cluster, node string }
	counts := map[nodeKey]int{}
	failed := map[nodeKey]int{}

	for i := range devices.Items {
		device := &devices.Items[i]
		cluster := device.Labels[simplyblockv1alpha2.DeviceLabelCluster]
		key := nodeKey{cluster: cluster, node: device.Spec.NodeRef}

		counts[key]++
		if device.Status.Phase == simplyblockv1alpha2.StorageDevicePhaseFailed {
			failed[key]++
		}

		if total := deviceTotalBytes(device); total > 0 {
			deviceCapacityBytes.WithLabelValues(cluster, device.Spec.NodeRef, device.Name).Set(float64(total))
		}
		c.publishPhase(device, cluster)
	}

	for key, count := range counts {
		nodeDeviceCount.WithLabelValues(key.cluster, key.node).Set(float64(count))
		nodeDeviceFailedCount.WithLabelValues(key.cluster, key.node).Set(float64(failed[key]))
	}

	return c.publishOccupancy(ctx, devices.Items)
}

// publishPhase writes the device's phase as one series per phase, so that every
// phase is a graphable time series and not a number a reader has to decode.
func (c *StorageDeviceCollector) publishPhase(
	device *simplyblockv1alpha2.StorageDevice, cluster string,
) {
	for _, phase := range []simplyblockv1alpha2.StorageDevicePhase{
		simplyblockv1alpha2.StorageDevicePhaseOnline,
		simplyblockv1alpha2.StorageDevicePhaseDegraded,
		simplyblockv1alpha2.StorageDevicePhaseUnknown,
		simplyblockv1alpha2.StorageDevicePhaseRemoved,
		simplyblockv1alpha2.StorageDevicePhaseFailed,
	} {
		value := 0.0
		if device.Status.Phase == phase {
			value = 1
		}
		devicePhaseState.
			WithLabelValues(cluster, device.Spec.NodeRef, device.Name, string(phase)).
			Set(value)
	}
}

// publishOccupancy reads what each device holds and publishes it, warning about
// the ones over their cluster's threshold.
//
// The query is per cluster and not per device, which is what keeps eight hundred
// objects across a hundred nodes affordable: one request answers for every
// device of a cluster.
func (c *StorageDeviceCollector) publishOccupancy(
	ctx context.Context, devices []simplyblockv1alpha2.StorageDevice,
) error {
	if c.Capacity == nil {
		return nil
	}
	log := logf.FromContext(ctx).WithName("storagedevice-collector")

	// Group by the backend cluster the devices were observed on, which is what
	// the capacity query is scoped by.
	byCluster := map[string][]*simplyblockv1alpha2.StorageDevice{}
	for i := range devices {
		device := &devices[i]
		if device.Status.ClusterID == "" {
			continue // never observed on a stream, so nothing to ask about
		}
		byCluster[device.Status.ClusterID] = append(byCluster[device.Status.ClusterID], device)
	}

	for clusterID, group := range byCluster {
		samples, err := c.Capacity.DeviceCapacity(ctx, clusterID)
		if err != nil {
			// One unreachable Prometheus costs the used size of one cluster's
			// devices and nothing else, so the pass continues rather than
			// returning: the gauges that do not come from it are already
			// published.
			log.Error(err, "reading device capacity", "clusterID", clusterID)
			continue
		}
		for _, device := range group {
			sample, ok := samples[device.Spec.DeviceID]
			if !ok || !sample.Sampled() {
				continue
			}
			cluster := device.Labels[simplyblockv1alpha2.DeviceLabelCluster]
			deviceUsedBytes.
				WithLabelValues(cluster, device.Spec.NodeRef, device.Name).
				Set(float64(sample.Used))
			c.warnIfNearlyFull(ctx, device, sample)
		}
	}
	return nil
}

// warnIfNearlyFull emits DeviceNearlyFull the first time a device crosses its
// cluster's warning threshold, and again only after it has dropped back below
// it. Warning on every tick would make the event stream a record of how often
// the collector ran rather than of what happened to the device.
func (c *StorageDeviceCollector) warnIfNearlyFull(
	ctx context.Context, device *simplyblockv1alpha2.StorageDevice, sample prometheus.Capacity,
) {
	total := sample.Total
	if total == 0 {
		total = deviceTotalBytes(device)
	}
	if total <= 0 {
		return // nothing to be a fraction of
	}

	percent := float64(sample.Used) / float64(total) * 100
	threshold := c.nearlyFullPercent(ctx, device)

	key := client.ObjectKeyFromObject(device).String()
	if percent < threshold {
		delete(c.full, key)
		return
	}
	if c.full[key] {
		return
	}
	if c.full == nil {
		c.full = map[string]bool{}
	}
	c.full[key] = true

	c.Recorder.Eventf(device, nil, corev1.EventTypeWarning, "DeviceNearlyFull", "DeviceNearlyFull",
		"the device holds %.0f%% of its %d bytes, over the cluster's %.0f%% warning threshold",
		percent, total, threshold)
}

// nearlyFullPercent is the warning threshold the device's own cluster declares,
// or DefaultNearlyFullPercent when it declares none. A cluster that cannot be
// read falls back to the default rather than skipping the check: the reason a
// device is full is not that its cluster object is missing.
func (c *StorageDeviceCollector) nearlyFullPercent(
	ctx context.Context, device *simplyblockv1alpha2.StorageDevice,
) float64 {
	name := device.Labels[simplyblockv1alpha2.DeviceLabelCluster]
	if name == "" {
		return DefaultNearlyFullPercent
	}

	var cluster simplyblockv1alpha1.StorageCluster
	key := client.ObjectKey{Namespace: device.Namespace, Name: name}
	if err := c.Get(ctx, key, &cluster); err != nil {
		if !apierrors.IsNotFound(err) {
			logf.FromContext(ctx).Error(err, "reading the cluster's warning threshold", "cluster", name)
		}
		return DefaultNearlyFullPercent
	}
	if cluster.Spec.WarningThresholdSpec == nil {
		return DefaultNearlyFullPercent
	}
	if percent := ptr.IntFromOrZero(cluster.Spec.WarningThresholdSpec.Capacity); percent > 0 {
		return float64(percent)
	}
	return DefaultNearlyFullPercent
}

// deviceTotalBytes is the device's size as its object reports it, or 0 when the
// object does not carry one yet.
func deviceTotalBytes(device *simplyblockv1alpha2.StorageDevice) int64 {
	if device.Status.Capacity == nil || device.Status.Capacity.TotalBytes == nil {
		return 0
	}
	return *device.Status.Capacity.TotalBytes
}
