package cluster

import (
	"net/http"
	"strings"
	"testing"

	atlascp "github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const gib = int64(1) << 30

// GrowDevices is one call whose answer is recorded on the operation: the
// factor every data device grew by, each device's new size, and the part of
// each LUN that stays unused.
func TestGrowDevicesRecordsTheFactorAndTheUnusedCapacity(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		growDevices: func(string) (atlascp.ClusterDeviceGrowth, error) {
			return atlascp.ClusterDeviceGrowth{
				Grown:  true,
				Factor: 1.5,
				Devices: []atlascp.DeviceGrowth{
					{DeviceID: "a", OldSize: 100 * gib, NewSize: 150 * gib, BackendSize: 150 * gib},
					{DeviceID: "b", OldSize: 100 * gib, NewSize: 150 * gib, BackendSize: 200 * gib,
						UnusedSize: 50 * gib},
				},
			}, nil
		},
	}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionGrowDevices)
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, newTestCluster(), ops)

	got, _ := reconcileOps(t, r, 6)

	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (message: %s)", got.Status.Phase, got.Status.Message)
	}
	if api.growDevicesCalls != 1 {
		t.Errorf("the devices were grown %d times, want once", api.growDevicesCalls)
	}
	g := got.Status.DeviceGrowth
	if g == nil || !g.Grown || g.Factor != "1.5" || len(g.Devices) != 2 || g.UnusedBytes != 50*gib {
		t.Fatalf("status.deviceGrowth = %+v, want grown by 1.5 with 50 GiB unused", g)
	}
	if g.Devices[1] != (simplyblockv1alpha2.DeviceGrowthStatus{DeviceID: "b", Size: 150 * gib, UnusedBytes: 50 * gib}) {
		t.Errorf("devices[1] = %+v, want b at 150 GiB with 50 GiB unused", g.Devices[1])
	}
	if !rec.has(DevicesGrown) || rec.has(DeviceShrunk) {
		t.Error("want DevicesGrown and no DeviceShrunk")
	}
}

// A block device that did not grow keeps every device at its size, which is
// an answer, not a failure: the LUNs have not all been grown yet.
func TestGrowDevicesWithoutGrowthSucceeds(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		growDevices: func(string) (atlascp.ClusterDeviceGrowth, error) {
			return atlascp.ClusterDeviceGrowth{
				Factor: 1, Reason: "Not every block device has grown",
				Devices: []atlascp.DeviceGrowth{{DeviceID: "a", OldSize: 100, NewSize: 100}},
			}, nil
		},
	}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionGrowDevices)
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, newTestCluster(), ops)

	got, _ := reconcileOps(t, r, 6)

	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", got.Status.Phase)
	}
	g := got.Status.DeviceGrowth
	if g == nil || g.Grown || g.Factor != "1" || !strings.Contains(g.Reason, "Not every") {
		t.Errorf("status.deviceGrowth = %+v, want not grown with the reason", g)
	}
	if !rec.has(DevicesNotGrown) {
		t.Error("want DevicesNotGrown")
	}
}

// A shrunk LUN is refused by the control plane, which grows nothing and
// raises its device_shrunk alert. The operation fails, records the device,
// and warns.
func TestShrunkDeviceFailsTheGrowth(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		growDevices: func(string) (atlascp.ClusterDeviceGrowth, error) {
			return atlascp.ClusterDeviceGrowth{
				Factor: 1,
				Reason: "A block device is smaller than its device; nothing was grown",
				Errors: map[string]string{"b": "The block device shrank"},
				Shrunk: []string{"b"},
			}, nil
		},
	}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionGrowDevices)
	rec := &recorder{}
	r := newOpsReconciler(t, api, rec, newTestCluster(), ops)

	got, cluster := reconcileOps(t, r, 6)

	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "smaller") {
		t.Errorf("message = %q, want the reason", got.Status.Message)
	}
	g := got.Status.DeviceGrowth
	if g == nil || g.Grown || len(g.Shrunk) != 1 || g.Shrunk[0] != "b" {
		t.Errorf("status.deviceGrowth = %+v, want b listed as shrunk", g)
	}
	if !rec.has(DeviceShrunk) {
		t.Error("a shrunk device emitted no DeviceShrunk")
	}
	if api.growDevicesCalls != 1 {
		t.Errorf("a refused growth was retried: %d calls", api.growDevicesCalls)
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("a failed operation kept the cluster locked by %q", cluster.Status.ActiveOpsRef)
	}
}

// A cluster that is not in lblk device mode has no devices to grow, and the
// control plane says so with a conflict, which ends the operation.
func TestRefusedClusterGrowthFails(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		growDevices: func(string) (atlascp.ClusterDeviceGrowth, error) {
			return atlascp.ClusterDeviceGrowth{}, &ControlPlaneError{
				Status: http.StatusConflict,
				Body:   `{"detail":"Online device growth needs a cluster in lblk device mode"}`,
			}
		},
	}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionGrowDevices)
	r := newOpsReconciler(t, api, &recorder{}, newTestCluster(), ops)

	got, cluster := reconcileOps(t, r, 6)

	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "lblk") {
		t.Errorf("message = %q, want the control plane's reason", got.Status.Message)
	}
	if api.growDevicesCalls != 1 {
		t.Errorf("a refusal was retried: %d calls", api.growDevicesCalls)
	}
	if cluster.Status.ActiveOpsRef != "" {
		t.Errorf("a failed operation kept the cluster locked by %q", cluster.Status.ActiveOpsRef)
	}
}
