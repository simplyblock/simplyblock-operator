// Tests for the verdict drawn from a device's health log.

package node

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func healthyNVMe() controlplane.DeviceHealth {
	return controlplane.DeviceHealth{
		Supported: true, Source: "spdk",
		CriticalWarning: ptr.To(0), TemperatureCelsius: ptr.To(38),
		AvailableSparePercent: ptr.To(100), AvailableSpareThresholdPercent: ptr.To(10),
		PercentageUsed: ptr.To(4), MediaErrors: ptr.To(0),
	}
}

func TestAHealthyNVMeLogIsOk(t *testing.T) {
	if v, reason := judgeSMART(healthyNVMe()); v != simplyblockv1alpha2.DeviceSMARTOk {
		t.Errorf("verdict = %s (%s), want Ok", v, reason)
	}
}

func TestTheNVMeCriticalWarningBitsDecide(t *testing.T) {
	for _, tc := range []struct {
		bits int
		want simplyblockv1alpha2.DeviceSMARTVerdict
		says string
	}{
		{0x01, simplyblockv1alpha2.DeviceSMARTFailing, "spare"},
		{0x02, simplyblockv1alpha2.DeviceSMARTWarning, "temperature"},
		{0x04, simplyblockv1alpha2.DeviceSMARTFailing, "reliability"},
		{0x08, simplyblockv1alpha2.DeviceSMARTFailing, "read-only"},
		{0x10, simplyblockv1alpha2.DeviceSMARTFailing, "volatile memory backup"},
		{0x06, simplyblockv1alpha2.DeviceSMARTFailing, "reliability"},
	} {
		h := healthyNVMe()
		h.CriticalWarning = ptr.To(tc.bits)
		v, reason := judgeSMART(h)
		if v != tc.want || !strings.Contains(reason, tc.says) {
			t.Errorf("bits %#x: %s (%s), want %s naming %q", tc.bits, v, reason, tc.want, tc.says)
		}
	}
}

func TestWearAndMediaErrorsAreWarnings(t *testing.T) {
	worn := healthyNVMe()
	worn.PercentageUsed = ptr.To(93)
	if v, _ := judgeSMART(worn); v != simplyblockv1alpha2.DeviceSMARTWarning {
		t.Errorf("93%% used = %s, want Warning", v)
	}

	errored := healthyNVMe()
	errored.MediaErrors = ptr.To(3)
	if v, reason := judgeSMART(errored); v != simplyblockv1alpha2.DeviceSMARTWarning || !strings.Contains(reason, "3 media errors") {
		t.Errorf("media errors = %s (%s), want Warning naming them", v, reason)
	}
}

// The device's own warning bit is set only once spare capacity is below the
// threshold; a log read in between, or from a device whose firmware does not
// set the bit, still reports both numbers.
func TestSpareAtTheThresholdIsFailing(t *testing.T) {
	h := healthyNVMe()
	h.AvailableSparePercent = ptr.To(10)
	if v, _ := judgeSMART(h); v != simplyblockv1alpha2.DeviceSMARTFailing {
		t.Errorf("spare at threshold = %s, want Failing", v)
	}
}

func TestASATADeviceThatFailsItsSelfAssessmentIsFailing(t *testing.T) {
	h := controlplane.DeviceHealth{Supported: true, Source: "smartctl", SMARTPassed: ptr.To(false)}
	if v, _ := judgeSMART(h); v != simplyblockv1alpha2.DeviceSMARTFailing {
		t.Errorf("verdict = %s, want Failing", v)
	}
}

func TestRemappedSectorsAreAWarning(t *testing.T) {
	h := controlplane.DeviceHealth{
		Supported: true, Source: "smartctl", SMARTPassed: ptr.To(true),
		ReallocatedSectors: ptr.To(8), PendingSectors: ptr.To(0),
	}
	if v, reason := judgeSMART(h); v != simplyblockv1alpha2.DeviceSMARTWarning || !strings.Contains(reason, "reallocated") {
		t.Errorf("verdict = %s (%s), want Warning naming the reallocated sectors", v, reason)
	}
}

func TestADeviceWithNoHealthLogIsUnsupported(t *testing.T) {
	if v, _ := judgeSMART(controlplane.DeviceHealth{Supported: false}); v != simplyblockv1alpha2.DeviceSMARTUnsupported {
		t.Errorf("verdict = %s, want Unsupported", v)
	}
}

// A log with nothing in it that can be judged is not a healthy log.
func TestALogWithNoMetricsIsUnknown(t *testing.T) {
	if v, _ := judgeSMART(controlplane.DeviceHealth{Supported: true, Source: "smartctl"}); v != simplyblockv1alpha2.DeviceSMARTUnknown {
		t.Errorf("verdict = %s, want Unknown", v)
	}
}

func TestTheStatusCarriesTheReadings(t *testing.T) {
	s := smartStatus(healthyNVMe())
	if s.Source != "spdk" || s.CriticalWarning == nil || *s.CriticalWarning != 0 ||
		s.AvailableSparePercent == nil || *s.AvailableSparePercent != 100 {
		t.Errorf("status = %+v", s)
	}
	if s.SMARTPassed != nil {
		t.Error("an NVMe log reported a SATA self-assessment")
	}
}
