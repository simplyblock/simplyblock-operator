// The device collector on one site of a stretched cluster. The peer site's
// devices are listed here as remote StorageDevices for visibility, and the peer
// site's own collector publishes them, so this site must not publish them too.

package node

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/simplyblock/atlas/prometheus"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// Regression: 2026-10-22-remote-devices-published-twice.
// The collector published the peer site's remote devices, so each of them had
// a size, phase, and used-bytes series on both sites, and a nearly full remote
// device was warned about by both sites' operators.
func TestARemoteDevicePublishesNoSample(t *testing.T) {
	capacity := &fakeCapacitySource{samples: map[string]map[string]prometheus.Capacity{
		sdCluster: {sdDevice: {Total: 1000, Used: 900, SampledAt: time.Unix(1, 0)}},
	}}
	remote := collectorDevice(
		"production-7f3a9c-5e0000a1", simplyblockv1alpha2.StorageDevicePhaseOnline, sdDevice, 1000)
	remote.Status.Remote = true
	c := newCollector(t, capacity, collectorCluster(75), remote)

	if err := c.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	for name, count := range map[string]int{
		"capacity":     testutil.CollectAndCount(deviceCapacityBytes),
		"phase":        testutil.CollectAndCount(devicePhaseState),
		"used-bytes":   testutil.CollectAndCount(deviceUsedBytes),
		"device count": testutil.CollectAndCount(nodeDeviceCount),
	} {
		if count != 0 {
			t.Errorf("%d %s series for a remote device, want none", count, name)
		}
	}
	if capacity.calls != 0 {
		t.Errorf("%d capacity queries for a site with only remote devices, want none",
			capacity.calls)
	}
	select {
	case event := <-recorderOf(t, c).Events:
		t.Errorf("an event for a remote device: %s", event)
	default:
	}
}
