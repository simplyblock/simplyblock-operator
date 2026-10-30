// Tests for the fleet comparison that decides whether a device is slow, and for
// the hysteresis that keeps a single burst from flipping the verdict.

package node

import (
	"testing"

	"github.com/simplyblock/atlas/prometheus"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const tib = int64(1) << 40

// fleetDevice is one device of a test fleet: its weight in TiB, its IOPS, and
// its mean latency per I/O in microseconds, on a 2 GHz tick clock.
func fleetDevice(id string, weightTiB float64, iops, latencyMicros float64) fleetSample {
	const hz = 2.0e9
	return fleetSample{
		ID:          id,
		WeightBytes: int64(weightTiB * float64(tib)),
		IO: prometheus.DeviceIO{
			WriteOpsPerSec:          iops,
			WriteBytesPerSec:        iops * 65536,
			WriteLatencyTicksPerSec: iops * latencyMicros * 1e-6 * hz,
			TickRateHz:              hz,
		},
	}
}

func verdictOf(t *testing.T, got map[string]fleetAssessment, id string) simplyblockv1alpha2.DevicePerformanceVerdict {
	t.Helper()
	a, ok := got[id]
	if !ok {
		t.Fatalf("no assessment for %s in %v", id, got)
	}
	return a.Verdict
}

func TestAHealthyFleetIsOk(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("a", 1, 400, 300),
		fleetDevice("b", 1, 420, 320),
		fleetDevice("c", 1, 390, 280),
		fleetDevice("d", 1, 410, 350),
	}, defaultFleetConfig())

	for _, id := range []string{"a", "b", "c", "d"} {
		if v := verdictOf(t, got, id); v != simplyblockv1alpha2.DevicePerformanceOk {
			t.Errorf("%s = %s (%s), want Ok", id, v, got[id].Reason)
		}
	}
}

// The case the feature exists for: a virtual disk whose host storage is slow
// serves the same share as its siblings while every I/O on it takes seconds.
func TestADeviceFarSlowerThanTheFleetAtTheSameLoadIsSlow(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("a", 1.5, 300, 1500000),
		fleetDevice("b", 1.5, 300, 4000),
		fleetDevice("c", 1.5, 310, 3500),
		fleetDevice("d", 1.5, 290, 4200),
	}, defaultFleetConfig())

	if v := verdictOf(t, got, "a"); v != simplyblockv1alpha2.DevicePerformanceSlow {
		t.Errorf("a = %s (%s), want Slow", v, got["a"].Reason)
	}
	if got["a"].LatencyRatio < 100 {
		t.Errorf("latency ratio = %v, want the device measured against the fleet median", got["a"].LatencyRatio)
	}
	for _, id := range []string{"b", "c", "d"} {
		if v := verdictOf(t, got, id); v != simplyblockv1alpha2.DevicePerformanceOk {
			t.Errorf("%s = %s, want Ok", id, v)
		}
	}
}

// A device carrying far more than its share queues, and its latency is the
// load's rather than the device's: a rebuild onto a returning node is exactly
// this, and calling the device slow would point somebody at healthy hardware.
func TestAHighLatencyExplainedByLoadIsNotSlow(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("a", 1, 1500, 9000),
		fleetDevice("b", 1, 300, 800),
		fleetDevice("c", 1, 300, 900),
		fleetDevice("d", 1, 300, 850),
	}, defaultFleetConfig())

	if v := verdictOf(t, got, "a"); v != simplyblockv1alpha2.DevicePerformanceOk {
		t.Errorf("a = %s (%s), want Ok: the device carries several times its share", v, got["a"].Reason)
	}
}

// A device that is not keeping up serves less than its share while it is
// slower than the rest, which is a saturated device whose backlog sits in the
// storage engine rather than in its latency alone.
func TestADeviceServingLessThanItsShareWhileSlowerIsSlow(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("a", 1, 100, 6000),
		fleetDevice("b", 1, 400, 3000),
		fleetDevice("c", 1, 400, 3100),
		fleetDevice("d", 1, 400, 2900),
	}, defaultFleetConfig())

	if v := verdictOf(t, got, "a"); v != simplyblockv1alpha2.DevicePerformanceSlow {
		t.Errorf("a = %s (%s), want Slow", v, got["a"].Reason)
	}
}

// A device that is new, or whose data has not been rebalanced onto it yet,
// serves less than its weight while being as fast as anything else. That is
// placement, not a fault.
func TestADeviceUnderItsShareAtNormalLatencyIsOk(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("a", 1, 40, 300),
		fleetDevice("b", 1, 400, 300),
		fleetDevice("c", 1, 400, 310),
		fleetDevice("d", 1, 400, 290),
	}, defaultFleetConfig())

	if v := verdictOf(t, got, "a"); v != simplyblockv1alpha2.DevicePerformanceOk {
		t.Errorf("a = %s (%s), want Ok", v, got["a"].Reason)
	}
}

// The share a device is entitled to follows its weight: a device twice the
// size of its siblings is expected to carry twice their I/O, and doing so is
// not above its share.
func TestTheShareFollowsTheWeight(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("big", 2, 800, 400),
		fleetDevice("b", 1, 400, 400),
		fleetDevice("c", 1, 400, 400),
		fleetDevice("d", 1, 400, 400),
	}, defaultFleetConfig())

	if r := got["big"].IOPSRatio; r < 99 || r > 101 {
		t.Errorf("IOPS ratio of a double-weight device carrying double IO = %v%%, want 100%%", r)
	}
}

// Several times the median but a fraction of a millisecond in absolute terms
// is a fast fleet with one slightly less fast device, which nobody can act on.
func TestARatioBelowTheAbsoluteFloorIsOk(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("a", 1, 400, 60),
		fleetDevice("b", 1, 400, 12),
		fleetDevice("c", 1, 400, 11),
		fleetDevice("d", 1, 400, 13),
	}, defaultFleetConfig())

	if v := verdictOf(t, got, "a"); v != simplyblockv1alpha2.DevicePerformanceOk {
		t.Errorf("a = %s (%s), want Ok below the absolute floor", v, got["a"].Reason)
	}
}

func TestAFleetTooSmallToCompareIsUnknown(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("a", 1, 400, 900000),
		fleetDevice("b", 1, 400, 300),
	}, defaultFleetConfig())

	for _, id := range []string{"a", "b"} {
		if v := verdictOf(t, got, id); v != simplyblockv1alpha2.DevicePerformanceUnknown {
			t.Errorf("%s = %s, want Unknown with two devices", id, v)
		}
	}
}

func TestAnIdleDeviceIsUnknown(t *testing.T) {
	got := assessFleet([]fleetSample{
		fleetDevice("idle", 1, 0.5, 900000),
		fleetDevice("b", 1, 400, 300),
		fleetDevice("c", 1, 400, 300),
		fleetDevice("d", 1, 400, 300),
	}, defaultFleetConfig())

	if v := verdictOf(t, got, "idle"); v != simplyblockv1alpha2.DevicePerformanceUnknown {
		t.Errorf("idle = %s, want Unknown", v)
	}
}

// Without a tick rate the latency cannot be put in seconds, but devices of one
// fleet can still be compared to each other in ticks, and a device a thousand
// times slower than its siblings is slow in any unit.
func TestWithoutATickRateTheRatioAloneDecides(t *testing.T) {
	samples := []fleetSample{
		fleetDevice("a", 1, 300, 1500000),
		fleetDevice("b", 1, 300, 1500),
		fleetDevice("c", 1, 300, 1500),
		fleetDevice("d", 1, 300, 1500),
	}
	for i := range samples {
		samples[i].IO.TickRateHz = 0
	}
	got := assessFleet(samples, defaultFleetConfig())

	if v := verdictOf(t, got, "a"); v != simplyblockv1alpha2.DevicePerformanceSlow {
		t.Errorf("a = %s (%s), want Slow", v, got["a"].Reason)
	}
	if got["a"].LatencySeconds != 0 {
		t.Errorf("latency in seconds = %v, want none claimed without a tick rate", got["a"].LatencySeconds)
	}
}

func TestAVerdictChangesOnlyAfterItHasHeld(t *testing.T) {
	tr := newVerdictTracker(3, 2)
	ok, slow, unknown := simplyblockv1alpha2.DevicePerformanceOk, simplyblockv1alpha2.DevicePerformanceSlow,
		simplyblockv1alpha2.DevicePerformanceUnknown

	steps := []struct {
		observed simplyblockv1alpha2.DevicePerformanceVerdict
		want     simplyblockv1alpha2.DevicePerformanceVerdict
	}{
		{ok, ok},      // the first reading of a healthy device is believed at once
		{slow, ok},    // one slow assessment is a burst
		{slow, ok},    // two is still not three
		{unknown, ok}, // an assessment with nothing to say neither helps nor resets
		{slow, slow},  // three
		{ok, slow},    // one good assessment does not clear it
		{slow, slow},  // and a relapse restarts the count
		{ok, slow},
		{ok, ok}, // two in a row do
	}
	for i, s := range steps {
		got, _ := tr.observe("dev", "", s.observed)
		if got != s.want {
			t.Fatalf("step %d: observed %s, stable = %s, want %s", i, s.observed, got, s.want)
		}
	}
}

// After a restart the tracker has no history, and the verdict the object
// already carries is the one to keep until the readings disagree for long
// enough: a slow device must not read as fine for the first minutes after the
// operator restarts.
func TestTheTrackerStartsFromThePublishedVerdict(t *testing.T) {
	tr := newVerdictTracker(3, 2)
	got, changed := tr.observe("dev", simplyblockv1alpha2.DevicePerformanceSlow, simplyblockv1alpha2.DevicePerformanceOk)
	if got != simplyblockv1alpha2.DevicePerformanceSlow || changed {
		t.Errorf("stable = %s, changed = %v, want the published Slow kept", got, changed)
	}
}
