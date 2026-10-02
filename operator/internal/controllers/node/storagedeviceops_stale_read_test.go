// Tests for a device operation's pass that reads the operation from a cache one
// write behind. The restart's write-ahead record, DeviceStatusBefore, was written
// through a write that rereads and retries on a conflict, so a pass holding a
// stale copy got its record through as well and then restarted the device a
// second time: a device recycled twice. The removal and the failure are guarded
// only by what the control plane reports, which has not moved yet when the
// second pass arrives.
//
// Regression: lblk_outage_matrix_k8s-20261002-111746, where the same read
// sent an Activate to the control plane twice, 32ms apart.

package node

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/controllers/testsupport"
)

func TestADevicePassReadingTheOperationBeforeTheCallDoesNotCallAgain(t *testing.T) {
	cases := []struct {
		name   string
		ops    func() *simplyblockv1alpha2.StorageDeviceOps
		status string
		calls  func(*deviceCalls) int
	}{
		{"Restart", deviceOperation, cpDeviceOnline,
			func(c *deviceCalls) int { return c.restarts }},
		{"Fail removing", failOperation, cpDeviceOnline,
			func(c *deviceCalls) int { return c.removals }},
		{"Fail failing", failOperation, cpDeviceRemoved,
			func(c *deviceCalls) int { return c.failures }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Frozen: the control plane accepted the call and has not
			// published its effect yet, which is the window a second pass
			// arrives in.
			api := &deviceCalls{status: tc.status, frozen: true}
			w := newDeviceWorld(t, api, deviceObject(), tc.ops())
			cache := &testsupport.LaggingClient{Client: w.r.Client}
			w.r.Client = cache

			var ops *simplyblockv1alpha2.StorageDeviceOps
			var device simplyblockv1alpha2.StorageDevice
			for range 8 {
				ops, _ = w.read()
				if err := w.c.Get(context.Background(), types.NamespacedName{
					Namespace: deviceOpsNamespace, Name: deviceObjectName,
				}, &device); err != nil {
					t.Fatalf("read the device: %v", err)
				}
				w.pass()
				if tc.calls(api) > 0 {
					break
				}
			}
			if got := tc.calls(api); got != 1 {
				t.Fatalf("the call was issued %d times on the way to it, want 1; issued: %v",
					got, api.issued)
			}

			cache.Lag(ops, 1)
			cache.Lag(&device, 10)
			w.pass()
			cache.CatchUp()

			if got := tc.calls(api); got != 1 {
				t.Errorf("the call was issued %d times, want 1: the second pass read the "+
					"operation before the first one made it; issued: %v", got, api.issued)
			}
		})
	}
}

// A pass that writes twice, recording the status of a device already out of
// the data path and then the step that follows, must not have its second write
// conflict with its first. The first write's version is the one the second
// patches against, and a cache that has not caught up cannot supply it.
func TestADevicePassThatWritesTwiceDoesNotConflictWithItself(t *testing.T) {
	api := &deviceCalls{status: cpDeviceRemoved, frozen: true}
	w := newDeviceWorld(t, api, deviceObject(), failOperation())
	cache := &testsupport.LaggingClient{Client: w.r.Client}
	w.r.Client = cache

	before := w.driveTo(stepDeviceRemoving)
	cache.Lag(before, 20)
	_, err := w.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceOpsName},
	})
	cache.CatchUp()
	if err != nil {
		t.Fatalf("the pass failed on its own write: %v", err)
	}

	ops, _ := w.read()
	if got := ops.Status.Step.State; got != string(stepDeviceFailing) {
		t.Errorf("step = %q, want Failing", got)
	}
	if got := ops.Status.DeviceStatusBefore; got != cpDeviceRemoved {
		t.Errorf("deviceStatusBefore = %q, want %q", got, cpDeviceRemoved)
	}
}
