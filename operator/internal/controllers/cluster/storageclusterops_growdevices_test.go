package cluster

import (
	"net/http"
	"strings"
	"testing"

	atlascp "github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// GrowDevices is one call whose answer is counted onto the operation: how many
// devices grew, how many had nothing to grow, and which could not.
func TestGrowDevicesRecordsTheOutcome(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		growDevices: func(string) (atlascp.ClusterDeviceGrowth, error) {
			return atlascp.ClusterDeviceGrowth{
				Devices: []atlascp.DeviceGrowth{
					{DeviceID: "a", OldSize: 100, NewSize: 200, Resized: true},
					{DeviceID: "b", OldSize: 200, NewSize: 200, Resized: false},
				},
				Errors:  map[string]string{"c": "Device must be online"},
				Uniform: true,
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
	if g == nil || g.Grown != 1 || g.Unchanged != 1 || g.Errors["c"] == "" || !g.Uniform {
		t.Errorf("status.deviceGrowth = %+v, want 1 grown, 1 unchanged, c failed, uniform", g)
	}
	if !rec.has(DevicesGrown) || rec.has(DeviceGrowthUneven) {
		t.Error("want DevicesGrown and no uneven-growth warning")
	}
	if len(api.growDevicesRebalance) != 1 || !api.growDevicesRebalance[0] {
		t.Errorf("rebalance flags = %v, want [true] by default", api.growDevicesRebalance)
	}
}

// After an uneven growth the control plane sets the weights to the new sizes
// and starts the rebalance; the operation records both and says so.
func TestUnevenClusterGrowthReweightsAndRebalances(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		growDevices: func(string) (atlascp.ClusterDeviceGrowth, error) {
			return atlascp.ClusterDeviceGrowth{
				Devices:   []atlascp.DeviceGrowth{{DeviceID: "a", Resized: true}},
				Uniform:   false,
				Rebalance: &atlascp.DeviceRebalance{WeightsUpdated: true, Started: true},
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
	if g == nil || !g.WeightsUpdated || !g.RebalanceStarted || g.Uniform {
		t.Errorf("status.deviceGrowth = %+v, want weights updated and rebalance started", g)
	}
	if !rec.has(DevicesReweighted) || rec.has(DeviceGrowthUneven) {
		t.Error("want DevicesReweighted and no uneven-growth warning")
	}
}

// A weight update the control plane could not do yet is a warning carrying its
// reason, and the operation still succeeds: the devices did grow.
func TestRefusedReweightWarnsWithTheReason(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		growDevices: func(string) (atlascp.ClusterDeviceGrowth, error) {
			return atlascp.ClusterDeviceGrowth{
				Devices: []atlascp.DeviceGrowth{{DeviceID: "a", Resized: true}},
				Uniform: false,
				Rebalance: &atlascp.DeviceRebalance{
					Reason: "Weights not updated: storage node n1 is in_restart"},
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
	if g == nil || g.WeightsUpdated || g.RebalanceStarted || !strings.Contains(g.RebalanceMessage, "in_restart") {
		t.Errorf("status.deviceGrowth = %+v, want the refusal reason", g)
	}
	if !rec.has(DeviceGrowthUneven) {
		t.Error("a refused weight update emitted no DeviceGrowthUneven")
	}
}

// spec.growDevices.skipRebalance asks the control plane to keep the weights.
func TestSkipRebalanceIsPassedOn(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
	}
	ops := newTestOps(simplyblockv1alpha2.StorageClusterOpsActionGrowDevices)
	ops.Spec.GrowDevices = &simplyblockv1alpha2.GrowDevicesSpec{SkipRebalance: true}
	r := newOpsReconciler(t, api, &recorder{}, newTestCluster(), ops)

	got, _ := reconcileOps(t, r, 6)

	if got.Status.Phase != simplyblockv1alpha2.StorageClusterOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if len(api.growDevicesRebalance) != 1 || api.growDevicesRebalance[0] {
		t.Errorf("rebalance flags = %v, want [false]", api.growDevicesRebalance)
	}
}

// Devices grown by different factors leave part of the new capacity unusable,
// which is a warning: the growth happened, and the remedy is growing the rest.
func TestUnevenClusterGrowthWarns(t *testing.T) {
	api := &fakeControlPlane{
		cluster: func(string) (atlascp.Cluster, error) { return activeCluster(), nil },
		growDevices: func(string) (atlascp.ClusterDeviceGrowth, error) {
			return atlascp.ClusterDeviceGrowth{
				Devices: []atlascp.DeviceGrowth{{DeviceID: "a", Resized: true}},
				Uniform: false,
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
	if !rec.has(DeviceGrowthUneven) {
		t.Error("an uneven growth emitted no DeviceGrowthUneven")
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
