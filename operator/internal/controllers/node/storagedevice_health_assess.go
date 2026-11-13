// The fleet comparison that decides whether a device is slow, and the
// hysteresis that decides when a verdict is believed.
//
// Both are pure: samples and history in, verdicts out. The monitor next door
// gathers the samples and publishes what is decided here, so every judgment
// this file makes can be tested against a hand-built fleet.
//
// A device is judged against the devices of its own cluster that carry the same
// role, never against a fixed number. What a healthy latency is depends on the
// hardware, the I/O size, and the load, and the fleet is the one reference that
// shares all three: the cluster stripes every volume across it, so each device
// sees the same mix in proportion to its weight. That weight is its capacity,
// because capacity is what the cluster map places data by.

package node

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/simplyblock/atlas/prometheus"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// fleetConfig holds the thresholds of the comparison. They are ratios to the
// fleet rather than absolute numbers, apart from the floor that keeps a fast
// fleet's small differences from reading as faults.
type fleetConfig struct {
	// LatencyFactor is how many times the fleet's median latency a device has
	// to reach to be slow at its normal share of the load.
	LatencyFactor float64

	// StarvedIOPSRatio and StarvedLatencyFactor describe a device that is not
	// keeping up: it serves no more than this fraction of its share while being
	// at least this many times slower than the fleet. A saturated device is
	// both, because the I/O it cannot serve waits in front of it.
	StarvedIOPSRatio     float64
	StarvedLatencyFactor float64

	// LoadExemptFactor is the multiple of its share above which a device's
	// latency is the load's: a device carrying a rebuild queues, and that is
	// not a fault of the device.
	LoadExemptFactor float64

	// MinExcessLatency is how much slower than the median, in absolute terms,
	// a device has to be before any ratio counts.
	MinExcessLatency time.Duration

	// MinDeviceIOPS is the load below which a device is not judged at all: a
	// mean over a handful of operations is noise.
	MinDeviceIOPS float64

	// MinFleetDevices is the smallest fleet a device is compared against.
	MinFleetDevices int
}

// defaultFleetConfig is the comparison the operator runs with. The factors are
// deliberately coarse: a device the alert fires for should be one somebody
// looking at the numbers would call slow without a second thought.
func defaultFleetConfig() fleetConfig {
	return fleetConfig{
		LatencyFactor:        3,
		StarvedIOPSRatio:     0.5,
		StarvedLatencyFactor: 1.5,
		LoadExemptFactor:     2,
		MinExcessLatency:     2 * time.Millisecond,
		MinDeviceIOPS:        5,
		MinFleetDevices:      3,
	}
}

// fleetSample is one device's reading, with the weight its share of the load
// is computed from.
type fleetSample struct {
	ID          string
	WeightBytes int64
	IO          prometheus.DeviceIO
}

// fleetAssessment is the verdict on one device and the measurements behind it.
// The ratios are percentages, which is what the device's status publishes.
type fleetAssessment struct {
	Verdict simplyblockv1alpha2.DevicePerformanceVerdict
	Reason  string

	// LatencySeconds and FleetLatencySeconds are zero when the fleet's latency
	// could only be compared in ticks.
	LatencySeconds      float64
	FleetLatencySeconds float64

	LatencyRatio float64
	IOPSRatio    float64
	measured     bool
}

// assessFleet judges every device of one fleet against the rest.
func assessFleet(samples []fleetSample, cfg fleetConfig) map[string]fleetAssessment {
	out := make(map[string]fleetAssessment, len(samples))

	// The devices that can be compared: weighted, and busy enough for a mean
	// latency to mean something.
	var active []fleetSample
	var totalIOPS, totalWeight float64
	allTimed := true
	for _, s := range samples {
		_, busy := s.IO.MeanLatencyTicks()
		if s.WeightBytes <= 0 || !busy || s.IO.IOPS() < cfg.MinDeviceIOPS {
			out[s.ID] = fleetAssessment{
				Verdict: simplyblockv1alpha2.DevicePerformanceUnknown,
				Reason:  "the device served too little IO over the window to be measured",
			}
			continue
		}
		active = append(active, s)
		totalIOPS += s.IO.IOPS()
		totalWeight += float64(s.WeightBytes)
		if s.IO.TickRateHz <= 0 {
			allTimed = false
		}
	}

	if len(active) < cfg.MinFleetDevices {
		for _, s := range active {
			out[s.ID] = fleetAssessment{
				Verdict: simplyblockv1alpha2.DevicePerformanceUnknown,
				Reason: fmt.Sprintf("only %d devices of this role carried IO, and at least %d are needed to compare",
					len(active), cfg.MinFleetDevices),
			}
		}
		return out
	}

	// Seconds when every device's clock is known, and ticks otherwise. Ticks
	// compare between devices on the same kind of CPU, and a fleet is usually
	// one kind, but no duration is claimed and the absolute floor is skipped.
	latency := func(s fleetSample) float64 {
		if allTimed {
			v, _ := s.IO.MeanLatencySeconds()
			return v
		}
		v, _ := s.IO.MeanLatencyTicks()
		return v
	}
	median := weightedMedianLatency(active, latency)
	sharePerByte := totalIOPS / totalWeight

	for _, s := range active {
		a := fleetAssessment{Verdict: simplyblockv1alpha2.DevicePerformanceOk, measured: true}
		lat := latency(s)
		latencyRatio := 0.0
		if median > 0 {
			latencyRatio = lat / median
		}
		iopsRatio := s.IO.IOPS() / (sharePerByte * float64(s.WeightBytes))
		a.LatencyRatio = latencyRatio * 100
		a.IOPSRatio = iopsRatio * 100
		if allTimed {
			a.LatencySeconds, a.FleetLatencySeconds = lat, median
		}

		excessOK := !allTimed || lat-median >= cfg.MinExcessLatency.Seconds()
		switch {
		case iopsRatio >= cfg.LoadExemptFactor:
			a.Reason = fmt.Sprintf("the device carries %.1f times its share of the IO, so its latency (%s the fleet median) is the load's",
				iopsRatio, ratioText(latencyRatio))
		case excessOK && latencyRatio >= cfg.LatencyFactor:
			a.Verdict = simplyblockv1alpha2.DevicePerformanceSlow
			a.Reason = fmt.Sprintf("mean latency %s is %s the fleet median of %s at %.0f%% of its share of the IO",
				latencyText(a.LatencySeconds, allTimed), ratioText(latencyRatio),
				latencyText(median, allTimed), iopsRatio*100)
		case excessOK && iopsRatio <= cfg.StarvedIOPSRatio && latencyRatio >= cfg.StarvedLatencyFactor:
			a.Verdict = simplyblockv1alpha2.DevicePerformanceSlow
			a.Reason = fmt.Sprintf("the device serves only %.0f%% of its share of the IO while its latency %s is %s the fleet median, so it is not keeping up",
				iopsRatio*100, latencyText(a.LatencySeconds, allTimed), ratioText(latencyRatio))
		default:
			a.Reason = fmt.Sprintf("latency %s the fleet median at %.0f%% of its share of the IO",
				ratioText(latencyRatio), iopsRatio*100)
		}
		out[s.ID] = a
	}
	return out
}

// weightedMedianLatency is the latency half of the fleet's I/O is served under.
// Each device counts by the I/O it served rather than once, so the median is that
// of an I/O rather than that of a device. It is the median rather than the mean
// so that the one device being judged cannot drag the reference toward itself.
func weightedMedianLatency(active []fleetSample, latency func(fleetSample) float64) float64 {
	type point struct{ latency, weight float64 }
	points := make([]point, 0, len(active))
	total := 0.0
	for _, s := range active {
		points = append(points, point{latency(s), s.IO.IOPS()})
		total += s.IO.IOPS()
	}
	sort.Slice(points, func(i, j int) bool { return points[i].latency < points[j].latency })
	half, run := total/2, 0.0
	for _, p := range points {
		run += p.weight
		if run >= half {
			return p.latency
		}
	}
	return points[len(points)-1].latency
}

// ratioText renders a latency ratio for a reason sentence.
func ratioText(r float64) string {
	if r >= 10 {
		return fmt.Sprintf("%.0f times", r)
	}
	return fmt.Sprintf("%.1f times", r)
}

// latencyText renders a latency for a reason sentence, or says that only a
// relative figure is known.
func latencyText(seconds float64, timed bool) string {
	if !timed {
		return "(in ticks, no tick rate exported)"
	}
	d := time.Duration(math.Round(seconds * float64(time.Second)))
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(10 * time.Microsecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

// verdictTracker holds a device's verdict steady until the assessments have
// disagreed with it for long enough. An assessment is one window's reading, and
// a burst that fills one window is not a slow device.
type verdictTracker struct {
	slowAfter, okAfter int
	devices            map[string]*trackedVerdict
}

type trackedVerdict struct {
	stable    simplyblockv1alpha2.DevicePerformanceVerdict
	candidate simplyblockv1alpha2.DevicePerformanceVerdict
	streak    int
}

// newVerdictTracker returns a tracker that turns a device slow after slowAfter
// consecutive slow assessments and clears it after okAfter consecutive good
// ones.
func newVerdictTracker(slowAfter, okAfter int) *verdictTracker {
	return &verdictTracker{slowAfter: slowAfter, okAfter: okAfter, devices: map[string]*trackedVerdict{}}
}

// observe records one assessment and returns the verdict to publish, and
// whether it changed.
//
// published is the verdict the object already carries, and seeds a device the
// tracker has not seen since it started, so an operator restart neither clears
// a slow device nor condemns a healthy one. An Unknown assessment says nothing
// about the device and leaves both the verdict and the count where they are.
func (t *verdictTracker) observe(
	id string, published, observed simplyblockv1alpha2.DevicePerformanceVerdict,
) (simplyblockv1alpha2.DevicePerformanceVerdict, bool) {
	d, seen := t.devices[id]
	if !seen {
		if published == "" {
			published = simplyblockv1alpha2.DevicePerformanceUnknown
		}
		d = &trackedVerdict{stable: published}
		t.devices[id] = d
	}
	if observed == simplyblockv1alpha2.DevicePerformanceUnknown || observed == "" {
		return d.stable, false
	}
	if observed == d.stable {
		d.candidate, d.streak = "", 0
		return d.stable, false
	}
	if observed != d.candidate {
		d.candidate, d.streak = observed, 0
	}
	d.streak++

	need := t.okAfter
	switch {
	case observed == simplyblockv1alpha2.DevicePerformanceSlow:
		need = t.slowAfter
	case d.stable == simplyblockv1alpha2.DevicePerformanceUnknown:
		// Nothing is being cleared, so a healthy first reading is believed.
		need = 1
	}
	if d.streak < need {
		return d.stable, false
	}
	d.stable, d.candidate, d.streak = observed, "", 0
	return d.stable, true
}

// forget drops every device not in keep, so the tracker does not grow with
// every device the fleet has ever held.
func (t *verdictTracker) forget(keep map[string]bool) {
	for id := range t.devices {
		if !keep[id] {
			delete(t.devices, id)
		}
	}
}
