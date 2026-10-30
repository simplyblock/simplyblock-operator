// A device's health: the health log it keeps about itself, and the assessment
// an outside assessor records back so the control plane can alert on it.
//
// They are one file because they are the two halves of one loop. The control
// plane reads the health log (through SPDK for a device it drives, through the
// storage node's agent for one the host kernel drives) but draws no conclusion
// from it, and it has no view of how a device performs against its fleet. The
// assessor reads the first, measures the second, and records the verdicts, which
// the control plane's alerts endpoint raises while they are fresh.

package controlplane

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// DeviceHealth is one device's health log as last read. Every metric is a
// pointer because which ones a device reports depends on how it is attached: an
// NVMe drive reports wear and spare capacity, a SATA or SAS drive its overall
// self-assessment and its sector counts, and a virtual disk nothing at all.
type DeviceHealth struct {
	// Supported is false for a device that keeps no health log, in which case
	// every metric is nil. That is an answer rather than an error.
	Supported bool

	// Source names what read the log: `spdk`, `nvme-cli`, or `smartctl`.
	Source string

	ModelNumber      string
	SerialNumber     string
	FirmwareRevision string

	// CriticalWarning is the NVMe critical-warning bit field.
	CriticalWarning                *int
	TemperatureCelsius             *int
	AvailableSparePercent          *int
	AvailableSpareThresholdPercent *int
	PercentageUsed                 *int
	MediaErrors                    *int
	NumErrorLogEntries             *int
	PowerOnHours                   *int
	PowerCycles                    *int
	UnsafeShutdowns                *int

	// SMARTPassed is a SATA or SAS device's overall self-assessment, nil on
	// NVMe.
	SMARTPassed        *bool
	ReallocatedSectors *int
	PendingSectors     *int
}

// DeviceHealthInfo reads one device's health log. It wraps errs.ErrNotFound for
// a device the control plane does not hold. A device with no health log is not
// an error: it is returned with Supported false.
func (c *Client) DeviceHealthInfo(ctx context.Context, clusterID, nodeID, deviceID string) (DeviceHealth, error) {
	cluster, node, device, err := parseDeviceIDs(clusterID, nodeID, deviceID)
	if err != nil {
		return DeviceHealth{}, err
	}
	resp, err := c.api.ClustersStorageNodesDevicesGetDeviceHealthInfoApiV2ClustersClusterIdStorageNodesStorageNodeIdDevicesDeviceIdHealthInfoGetWithResponse(
		ctx, cluster, node, device)
	if err != nil {
		return DeviceHealth{}, fmt.Errorf("read the health log of device %s: %w", deviceID, err)
	}
	d, err := payload("health log of device "+deviceID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return DeviceHealth{}, err
	}
	return DeviceHealth{
		// A control plane older than the field answers only for devices that
		// have a log, so its absence is not `unsupported`.
		Supported:                      d.Supported == nil || *d.Supported,
		Source:                         deref(d.Source),
		ModelNumber:                    deref(d.ModelNumber),
		SerialNumber:                   deref(d.SerialNumber),
		FirmwareRevision:               deref(d.FirmwareRevision),
		CriticalWarning:                d.CriticalWarning,
		TemperatureCelsius:             d.TemperatureCelsius,
		AvailableSparePercent:          d.AvailableSparePercentage,
		AvailableSpareThresholdPercent: d.AvailableSpareThresholdPercentage,
		PercentageUsed:                 d.PercentageUsed,
		MediaErrors:                    d.MediaErrors,
		NumErrorLogEntries:             d.NumErrLogEntries,
		PowerOnHours:                   d.PowerOnHours,
		PowerCycles:                    d.PowerCycles,
		UnsafeShutdowns:                d.UnsafeShutdowns,
		SMARTPassed:                    d.SmartPassed,
		ReallocatedSectors:             d.ReallocatedSectors,
		PendingSectors:                 d.PendingSectors,
	}, nil
}

// DeviceHealthAssessment is a verdict on one device, recorded with the control
// plane so that its alerts endpoint raises device_slow and device_unhealthy.
type DeviceHealthAssessment struct {
	// Reporter names the assessor.
	Reporter string
	// ReportedAt and Valid bound how long the assessment is believed: an
	// assessor that stops reporting must not leave an alert firing forever, so
	// the control plane stops raising one once ReportedAt+Valid has passed.
	ReportedAt time.Time
	Valid      time.Duration

	// Performance and SMART are each nil when that half was not assessed.
	Performance *DevicePerformanceAssessment
	SMART       *DeviceSMARTAssessment
}

// DevicePerformanceAssessment is the device measured against its fleet. The
// verdict is the control plane's spelling: `ok`, `slow`, or `unknown`.
type DevicePerformanceAssessment struct {
	Verdict            string
	Reason             string
	Since              *time.Time
	LatencyMicros      *float64
	FleetLatencyMicros *float64
	LatencyRatio       *float64
	IOPSPerTiB         *float64
	FleetIOPSPerTiB    *float64
	IOPSRatio          *float64
}

// DeviceSMARTAssessment is the verdict on the device's health log: `ok`,
// `warning`, `failing`, `unsupported`, or `unknown`.
type DeviceSMARTAssessment struct {
	Verdict   string
	Reason    string
	CheckedAt *time.Time
}

// ReportDeviceHealthAssessment records an assessment of one device, replacing
// whatever was recorded before.
func (c *Client) ReportDeviceHealthAssessment(
	ctx context.Context, clusterID, nodeID, deviceID string, a DeviceHealthAssessment,
) error {
	cluster, node, device, err := parseDeviceIDs(clusterID, nodeID, deviceID)
	if err != nil {
		return err
	}
	valid := int(math.Ceil(a.Valid.Seconds()))
	body := cpapi.DeviceHealthAssessmentDTO{
		ReportedAt:   a.ReportedAt.UTC(),
		ValidSeconds: &valid,
	}
	if a.Reporter != "" {
		body.Reporter = &a.Reporter
	}
	if p := a.Performance; p != nil {
		body.Performance = &cpapi.DevicePerformanceAssessmentDTO{
			Verdict:         cpapi.DevicePerformanceAssessmentDTOVerdict(p.Verdict),
			Reason:          nonEmpty(p.Reason),
			Since:           p.Since,
			LatencyUs:       f32(p.LatencyMicros),
			FleetLatencyUs:  f32(p.FleetLatencyMicros),
			LatencyRatio:    f32(p.LatencyRatio),
			IopsPerTib:      f32(p.IOPSPerTiB),
			FleetIopsPerTib: f32(p.FleetIOPSPerTiB),
			IopsRatio:       f32(p.IOPSRatio),
		}
	}
	if s := a.SMART; s != nil {
		body.Smart = &cpapi.DeviceSmartAssessmentDTO{
			Verdict:   cpapi.DeviceSmartAssessmentDTOVerdict(s.Verdict),
			Reason:    nonEmpty(s.Reason),
			CheckedAt: s.CheckedAt,
		}
	}
	resp, err := c.api.ClustersStorageNodesDevicesSetHealthAssessmentApiV2ClustersClusterIdStorageNodesStorageNodeIdDevicesDeviceIdHealthAssessmentPutWithResponse(
		ctx, cluster, node, device, body)
	if err != nil {
		return fmt.Errorf("report the health of device %s: %w", deviceID, err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		return respError("report the health of device "+deviceID, code, resp.Body)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// f32 narrows an optional measurement to the generated client's float32.
func f32(v *float64) *float32 {
	if v == nil {
		return nil
	}
	f := float32(*v)
	return &f
}
