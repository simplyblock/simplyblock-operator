package node

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func growOperation() *simplyblockv1alpha2.StorageDeviceOps {
	ops := deviceOperation()
	ops.Spec.Action = simplyblockv1alpha2.StorageDeviceOpsActionGrow
	return ops
}

// A growth is one call whose answer is recorded on the operation, and the
// operation succeeds once the device serves at the new size.
func TestGrowRecordsTheGrowth(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline, growth: controlplane.DeviceGrowth{
		DeviceID: deviceBackendID, OldSize: 100, NewSize: 200, PlacementSize: 100,
		Resized: true, Uniform: true,
	}}
	w := newDeviceWorld(t, api, growOperation(), deviceObject())

	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded: %s", ops.Status.Phase, ops.Status.Message)
	}
	if api.grows != 1 {
		t.Errorf("the device was grown %d times, want once", api.grows)
	}
	want := simplyblockv1alpha2.StorageDeviceGrowth{
		OldSizeBytes: 100, NewSizeBytes: 200, PlacementSizeBytes: 100, Uniform: true,
	}
	if ops.Status.Growth == nil || *ops.Status.Growth != want {
		t.Errorf("status.growth = %+v, want %+v", ops.Status.Growth, want)
	}
	if !w.events.has(DeviceGrown) || w.events.has(DeviceGrowthUneven) {
		t.Errorf("events = %v, want DeviceGrown and no uneven-growth warning", w.events.reasons)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("the device is still locked by %q", device.Status.ActiveOpsRef)
	}
}

// Growing some devices of a cluster and not others leaves the added capacity
// partly unusable, which is worth a warning rather than a failure: the growth
// itself happened, and the other devices are grown by operations of their own.
func TestUnevenGrowthWarns(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline, growth: controlplane.DeviceGrowth{
		OldSize: 100, NewSize: 200, PlacementSize: 100, Resized: true,
		Uniform: false, Warning: "grown unevenly",
	}}
	w := newDeviceWorld(t, api, growOperation(), deviceObject())

	ops, _ := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", ops.Status.Phase)
	}
	if !w.events.has(DeviceGrowthUneven) {
		t.Errorf("events = %v, want DeviceGrowthUneven", w.events.reasons)
	}
	if ops.Status.Growth == nil || ops.Status.Growth.Warning != "grown unevenly" {
		t.Errorf("status.growth = %+v, want the warning recorded", ops.Status.Growth)
	}
}

// A block device that has not grown is nothing to grow, and an operation that
// finds so has still done what it was asked.
func TestGrowWithNothingToGrowSucceeds(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline, growth: controlplane.DeviceGrowth{
		OldSize: 100, NewSize: 100, PlacementSize: 100, Resized: false, Uniform: true,
	}}
	w := newDeviceWorld(t, api, growOperation(), deviceObject())

	ops, _ := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", ops.Status.Phase)
	}
	if w.events.has(DeviceGrown) {
		t.Errorf("a device that did not grow was reported grown")
	}
}

// A refusal (a shrunken block device, a device that is not lblk) is final, and
// the operation fails with the control plane's reason.
func TestRefusedGrowFails(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline, refuseGrow: &controlplane.StatusError{
		Op: "grow device", StatusCode: http.StatusConflict,
		Body: `{"detail":"The block device shrank"}`,
	}}
	w := newDeviceWorld(t, api, growOperation(), deviceObject())

	ops, device := w.settle()

	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "shrank") {
		t.Errorf("message = %q, want the control plane's reason", ops.Status.Message)
	}
	if api.grows != 1 {
		t.Errorf("a refusal was retried: %d calls", api.grows)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("a failed operation kept the device locked by %q", device.Status.ActiveOpsRef)
	}
}

// A transport failure is not a refusal: the operation stays running and asks
// again, since the call is safe to repeat.
func TestGrowRetriesATransportFailure(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline, refuseGrow: errors.New("connection reset")}
	w := newDeviceWorld(t, api, growOperation(), deviceObject())

	for range 6 {
		// A transport failure is returned to the controller runtime, which
		// requeues with backoff; the error itself is the expected outcome.
		_, _ = w.r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: deviceOpsNamespace, Name: deviceOpsName},
		})
	}
	ops, _ := w.read()

	if ops.Status.Phase == simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Fatalf("a transport failure failed the operation: %s", ops.Status.Message)
	}
	if api.grows < 2 {
		t.Errorf("the growth was asked %d times, want it asked again", api.grows)
	}
}

// Grow's one step is abortable, so it is not in the table a DELETE guard
// refuses from.
func TestGrowAddsNoUnabortableStep(t *testing.T) {
	for _, step := range UnabortableDeviceSteps() {
		if step == stepDeviceRequesting {
			t.Errorf("Requesting became unabortable")
		}
	}
}
