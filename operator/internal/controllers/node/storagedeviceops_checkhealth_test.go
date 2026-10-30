// Tests for the CheckHealth action: the health log read now rather than at the
// next scheduled read.

package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// healthChecks is a health monitor that records the devices it was asked about
// and answers with a fixed verdict or error.
type healthChecks struct {
	asked   []string
	verdict simplyblockv1alpha2.DeviceSMARTVerdict
	err     error
}

func (h *healthChecks) CheckHealth(
	_ context.Context, device *simplyblockv1alpha2.StorageDevice,
) (*simplyblockv1alpha2.DeviceSMART, error) {
	h.asked = append(h.asked, device.Name)
	if h.err != nil {
		return nil, h.err
	}
	return &simplyblockv1alpha2.DeviceSMART{Verdict: h.verdict, Reason: "the health log reports nothing wrong"}, nil
}

func checkHealthOperation() *simplyblockv1alpha2.StorageDeviceOps {
	ops := deviceOperation()
	ops.Spec.Action = simplyblockv1alpha2.StorageDeviceOpsActionCheckHealth
	return ops
}

func TestCheckHealthReadsTheLogOnceAndSucceeds(t *testing.T) {
	api := &deviceCalls{status: cpDeviceOnline}
	w := newDeviceWorld(t, api, deviceObject(), checkHealthOperation())
	checks := &healthChecks{verdict: simplyblockv1alpha2.DeviceSMARTOk}
	w.r.Health = checks

	ops, device := w.settle()
	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseSucceeded {
		t.Fatalf("phase = %s (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if len(checks.asked) != 1 {
		t.Errorf("the health log was read %d times, want once", len(checks.asked))
	}
	if !strings.Contains(ops.Status.Message, "Ok") {
		t.Errorf("message = %q, want the verdict in it", ops.Status.Message)
	}
	if len(api.issued) != 0 {
		t.Errorf("a health check issued %v against the device, want nothing", api.issued)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released", device.Status.ActiveOpsRef)
	}
}

func TestCheckHealthFailsWhenTheLogCannotBeRead(t *testing.T) {
	w := newDeviceWorld(t, &deviceCalls{status: cpDeviceOnline}, deviceObject(), checkHealthOperation())
	w.r.Health = &healthChecks{err: errors.New("storage node unreachable")}

	ops, device := w.settle()
	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Fatalf("phase = %s, want Failed", ops.Status.Phase)
	}
	if !strings.Contains(ops.Status.Message, "storage node unreachable") {
		t.Errorf("message = %q, want the read's error", ops.Status.Message)
	}
	if device.Status.ActiveOpsRef != "" {
		t.Errorf("activeOpsRef = %q, want the lock released", device.Status.ActiveOpsRef)
	}
}

func TestCheckHealthWithNoMonitorIsRefused(t *testing.T) {
	w := newDeviceWorld(t, &deviceCalls{status: cpDeviceOnline}, deviceObject(), checkHealthOperation())

	ops, _ := w.settle()
	if ops.Status.Phase != simplyblockv1alpha2.StorageDeviceOpsPhaseFailed {
		t.Fatalf("phase = %s, want Failed", ops.Status.Phase)
	}
}
