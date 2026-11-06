// The node resolving a suspended mapping on its own: when the deadline passes
// with nobody resuming it, and when the plugin restarts with a mapping still
// suspended in the kernel (design-cg-live-subsystem-flip.md §6.5 and §6.6).

package swaprpc

import (
	"testing"
	"time"

	"github.com/simplyblock/atlas/volstack/layers"
)

// fire suspends volA, prepared toward the target, and lets the deadline and the
// grace pass with nobody resuming it.
func (r *rig) fire() {
	r.t.Helper()
	if _, err := r.swapper.Prepare(ctx, []string{handle(volA)}, targetNQN, targetPaths()); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(10*time.Second)); err != nil {
		r.t.Fatal(err)
	}
}

// U-39
func TestTheWatchdogResumesOnTheTargetWhenTheNamespaceMoved(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	r.fire()
	r.moveNamespace(volA, "259:7", 3)

	r.clock.advance(15 * time.Second)

	name := layers.DMLinearName(volA)
	if r.mapper.isSuspended(name) || r.mapper.deviceOf(name) != "259:7" {
		t.Fatalf("suspended %t on %s; want resumed on the target's 259:7", r.mapper.isSuspended(name), r.mapper.deviceOf(name))
	}
	if r.fabricOf(volA).NQN != targetNQN {
		t.Fatal("the record still names the source")
	}
	if r.metrics.watchdog["target"] != 1 {
		t.Fatalf("watchdog outcomes = %v", r.metrics.watchdog)
	}
}

// U-40
func TestTheWatchdogResumesOnTheSourceWhenTheNamespaceNeverLeft(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	r.fire()

	r.clock.advance(15 * time.Second)

	name := layers.DMLinearName(volA)
	if r.mapper.isSuspended(name) || r.mapper.deviceOf(name) != "259:1" {
		t.Fatalf("want resumed on the source's 259:1")
	}
	if r.metrics.watchdog["source"] != 1 {
		t.Fatalf("watchdog outcomes = %v", r.metrics.watchdog)
	}
	if len(r.events.warnings) != 1 || r.events.warnings[0].reason != "SubsystemSwapWatchdogFired" {
		t.Fatalf("warnings = %+v, want one SubsystemSwapWatchdogFired", r.events.warnings)
	}
}

// U-41: an I/O error is a failure the application can handle, and a hang
// forever is not.
func TestTheWatchdogResumesOnTheCurrentTableWhenTheNamespaceIsNowhere(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	r.fire()
	r.devices.remove(sourceNQN)

	r.clock.advance(15 * time.Second)

	name := layers.DMLinearName(volA)
	if r.mapper.isSuspended(name) {
		t.Fatal("the mapping was left suspended")
	}
	if r.mapper.deviceOf(name) != "259:1" {
		t.Fatalf("re-pointed at %s with no namespace to point at", r.mapper.deviceOf(name))
	}
	if r.metrics.watchdog["none"] != 1 {
		t.Fatalf("watchdog outcomes = %v", r.metrics.watchdog)
	}
	if len(r.events.warnings) != 1 || r.events.warnings[0].reason != "SubsystemSwapWatchdogFired" ||
		r.events.warnings[0].handle != handle(volA) {
		t.Fatalf("warnings = %+v", r.events.warnings)
	}
}

// U-42 is TestSuspendHoldsEveryNamedMappingAndStartsTheWatchdogAfterTheGrace.

// U-43
func TestAResumeBeforeTheDeadlineStopsTheWatchdog(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	r.fire()
	r.moveNamespace(volA, "259:7", 3)
	if _, err := r.swapper.Resume(ctx, []string{handle(volA)}, targetNQN); err != nil {
		t.Fatal(err)
	}
	before := len(r.mapper.mutations())

	r.clock.advance(time.Minute)

	if len(r.mapper.mutations()) != before || len(r.events.warnings) != 0 || len(r.metrics.watchdog) != 0 {
		t.Fatalf("the watchdog acted after a resume: %v %v", r.mapper.mutations(), r.events.warnings)
	}
}

// U-44
func TestTheStartupScanResolvesEverySuspendedMappingAsTheWatchdogWould(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	r.stage(volB, "259:2", true)
	// A previous process suspended both and died. volA's namespace moved,
	// volB's did not.
	r.mapper.maps[layers.DMLinearName(volA)].suspended = true
	r.mapper.maps[layers.DMLinearName(volB)].suspended = true
	r.devices.remove(sourceNQN)
	r.devices.add(targetNQN, volA, "259:7", 3)
	r.devices.add(sourceNQN, volB, "259:2", 1)

	if err := r.swapper.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if r.mapper.isSuspended(layers.DMLinearName(volA)) || r.mapper.deviceOf(layers.DMLinearName(volA)) != "259:7" {
		t.Fatal("volA was not resumed on the subsystem holding its namespace")
	}
	if r.fabricOf(volA).NQN != targetNQN {
		t.Fatal("volA's record still names the source")
	}
	if r.mapper.isSuspended(layers.DMLinearName(volB)) || r.mapper.deviceOf(layers.DMLinearName(volB)) != "259:2" {
		t.Fatal("volB was not resumed on its source")
	}
}

// U-45
func TestTheStartupScanLeavesAMappingItDoesNotOwnAlone(t *testing.T) {
	r := newRig(t)
	r.mapper.maps["vdo--x-vpool"] = &mapping{device: "253:4", suspended: true}

	if err := r.swapper.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.mapper.isSuspended("vdo--x-vpool") || len(r.mapper.mutations()) != 0 {
		t.Fatalf("touched a mapping that is not the indirection's: %v", r.mapper.mutations())
	}
}

// U-46
func TestTheStartupScanWithNothingSuspendedChangesNothing(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)

	if err := r.swapper.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.mapper.mutations()) != 0 {
		t.Fatalf("mutations = %v", r.mapper.mutations())
	}
}
