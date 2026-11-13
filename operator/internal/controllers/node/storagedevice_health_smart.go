// The verdict drawn from a device's health log, and the status it is
// published as.
//
// The control plane reads the log and draws no conclusion from it, so the
// judgment is made here. It is deliberately conservative about Failing: that
// verdict raises a critical alert and is read as an order to replace the drive, so only
// what the device itself declares about its reliability reaches it. Wear,
// temperature, and error counts that a drive keeps serving through are
// Warnings.

package node

import (
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// wornPercentUsed is the share of its rated life at which a device is worth
// planning a replacement for. A device keeps working past 100, so this is a
// warning and never a failure.
const wornPercentUsed = 90

// The NVMe critical-warning bits (NVMe Base Specification, SMART / Health
// Information log page, byte 0), with what each says and whether it is a
// failure. Temperature is the one a device recovers from by itself.
var nvmeCriticalWarnings = []struct {
	bit     int
	what    string
	failing bool
}{
	{0x01, "available spare is below the device's threshold", true},
	{0x02, "temperature is outside the device's limits", false},
	{0x04, "the device reports its reliability as degraded", true},
	{0x08, "the device has gone read-only", true},
	{0x10, "the volatile memory backup has failed", true},
	{0x20, "the persistent memory region has gone read-only", true},
}

// judgeSMART returns the verdict on one health log and the reasons for it.
func judgeSMART(h controlplane.DeviceHealth) (simplyblockv1alpha2.DeviceSMARTVerdict, string) {
	if !h.Supported {
		return simplyblockv1alpha2.DeviceSMARTUnsupported, "the device keeps no health log the host can read"
	}

	var failing, warning []string
	judged := false

	if cw := h.CriticalWarning; cw != nil {
		judged = true
		for _, w := range nvmeCriticalWarnings {
			if *cw&w.bit == 0 {
				continue
			}
			if w.failing {
				failing = append(failing, w.what)
			} else {
				warning = append(warning, w.what)
			}
		}
	}
	if spare, threshold := h.AvailableSparePercent, h.AvailableSpareThresholdPercent; spare != nil && threshold != nil {
		judged = true
		if *threshold > 0 && *spare <= *threshold && (h.CriticalWarning == nil || *h.CriticalWarning&0x01 == 0) {
			failing = append(failing, fmt.Sprintf("available spare %d%% is at or below the threshold of %d%%", *spare, *threshold))
		}
	}
	if used := h.PercentageUsed; used != nil {
		judged = true
		if *used >= wornPercentUsed {
			warning = append(warning, fmt.Sprintf("%d%% of the rated life is used", *used))
		}
	}
	if n := h.MediaErrors; n != nil {
		judged = true
		if *n > 0 {
			warning = append(warning, fmt.Sprintf("%d media errors reported", *n))
		}
	}
	if passed := h.SMARTPassed; passed != nil {
		judged = true
		if !*passed {
			failing = append(failing, "the device fails its own SMART self-assessment")
		}
	}
	if n := h.ReallocatedSectors; n != nil {
		judged = true
		if *n > 0 {
			warning = append(warning, fmt.Sprintf("%d reallocated sectors", *n))
		}
	}
	if n := h.PendingSectors; n != nil {
		judged = true
		if *n > 0 {
			warning = append(warning, fmt.Sprintf("%d sectors pending reallocation", *n))
		}
	}

	switch {
	case len(failing) > 0:
		return simplyblockv1alpha2.DeviceSMARTFailing, strings.Join(append(failing, warning...), "; ")
	case len(warning) > 0:
		return simplyblockv1alpha2.DeviceSMARTWarning, strings.Join(warning, "; ")
	case !judged:
		return simplyblockv1alpha2.DeviceSMARTUnknown, "the health log carries nothing that can be judged"
	default:
		return simplyblockv1alpha2.DeviceSMARTOk, "the health log reports nothing wrong"
	}
}

// smartStatus is the status a health log is published as, with its verdict.
// CheckedAt is the caller's to set, because it is also set when a read fails.
func smartStatus(h controlplane.DeviceHealth) *simplyblockv1alpha2.DeviceSMART {
	verdict, reason := judgeSMART(h)
	return &simplyblockv1alpha2.DeviceSMART{
		Verdict:                        verdict,
		Reason:                         reason,
		Source:                         h.Source,
		CriticalWarning:                int32Of(h.CriticalWarning),
		TemperatureCelsius:             int32Of(h.TemperatureCelsius),
		AvailableSparePercent:          int32Of(h.AvailableSparePercent),
		AvailableSpareThresholdPercent: int32Of(h.AvailableSpareThresholdPercent),
		PercentageUsed:                 int32Of(h.PercentageUsed),
		MediaErrors:                    int64Of(h.MediaErrors),
		PowerOnHours:                   int64Of(h.PowerOnHours),
		UnsafeShutdowns:                int64Of(h.UnsafeShutdowns),
		SMARTPassed:                    h.SMARTPassed,
		ReallocatedSectors:             int64Of(h.ReallocatedSectors),
		PendingSectors:                 int64Of(h.PendingSectors),
	}
}

func int32Of(v *int) *int32 {
	if v == nil {
		return nil
	}
	return ptr.To(int32(*v))
}

func int64Of(v *int) *int64 {
	if v == nil {
		return nil
	}
	return ptr.To(int64(*v))
}
