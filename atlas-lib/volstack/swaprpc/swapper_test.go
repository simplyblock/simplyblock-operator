// What the node does for each call of the swap protocol
// (design-cg-live-subsystem-flip.md §6.2 to §6.4). The scenario IDs are those
// of test-plan-cg-live-subsystem-flip.md.

package swaprpc

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack/layers"
)

const (
	volA = "6f0c2b7e-0b8e-4b7e-9d0a-3c1f8f6a2d11"
	volB = "7a1d3c8f-1c9f-4c8f-8e1b-4d2a9f7b3e22"
)

var ctx = context.Background()

// U-22
func TestDescribeReportsAStagedIndirectVolumeAsSwappableAndWhereItPoints(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)

	got, err := r.swapper.Describe(ctx, []string{handle(volA)})
	if err != nil {
		t.Fatal(err)
	}
	want := VolumeState{Handle: handle(volA), Staged: true, Swappable: true, NQN: sourceNQN, NamespaceUUID: volA}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Describe = %+v, want %+v", got, want)
	}
}

// U-23
func TestDescribeReportsADirectVolumeAsNotSwappableAndAnUnknownOneAsNotStaged(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", false)

	got, err := r.swapper.Describe(ctx, []string{handle(volA), handle(volB)})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Staged || got[0].Swappable {
		t.Errorf("direct volume: %+v, want staged and not swappable", got[0])
	}
	if got[1].Staged || got[1].Swappable {
		t.Errorf("unknown volume: %+v, want not staged", got[1])
	}
}

func targetPaths() []nvmeof.Target {
	return []nvmeof.Target{
		{Transport: nvmeof.TransportTCP, Address: "10.0.0.1", Port: 4420},
		{Transport: nvmeof.TransportTCP, Address: "10.0.0.2", Port: 4420},
	}
}

// U-24
func TestPrepareConnectsTheTargetPathsInOrderWithTheNodesIdentity(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)

	if _, err := r.swapper.Prepare(ctx, []string{handle(volA)}, targetNQN, targetPaths()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.swapper.Prepare(ctx, []string{handle(volA)}, targetNQN, targetPaths()); err != nil {
		t.Fatal(err)
	}
	if len(r.connector.connected) == 0 {
		t.Fatal("nothing was connected")
	}
	paths := r.connector.connected[0]
	if len(paths) != 2 || paths[0].Address != "10.0.0.1" || paths[1].Address != "10.0.0.2" {
		t.Fatalf("paths = %+v, want the given order", paths)
	}
	for _, p := range paths {
		if p.NQN != targetNQN || p.HostNQN != "nqn.2014-08.org.nvmexpress:uuid:host" || p.HostID != "host" {
			t.Errorf("path %+v lacks the target NQN or the node's identity", p)
		}
	}
	if len(r.mapper.mutations()) != 0 {
		t.Errorf("Prepare touched a mapping: %v", r.mapper.mutations())
	}
}

// U-25
func TestPrepareThatCannotReachTheTargetFailsAndLeavesTheSourceAlone(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	r.connector.err = errors.New("no route to host")

	if _, err := r.swapper.Prepare(ctx, []string{handle(volA)}, targetNQN, targetPaths()); err == nil {
		t.Fatal("an unreachable target was reported prepared")
	}
	if len(r.connector.disconnected) != 0 || len(r.mapper.mutations()) != 0 {
		t.Fatalf("the source was touched: disconnects %v, mapper %v", r.connector.disconnected, r.mapper.mutations())
	}
}

// U-26
func TestSuspendHoldsEveryNamedMappingAndStartsTheWatchdogAfterTheGrace(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	r.stage(volB, "259:2", true)

	deadline := r.deadline(10 * time.Second)
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA), handle(volB)}, deadline); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{volA, volB} {
		if !r.mapper.isSuspended(layers.DMLinearName(uuid)) {
			t.Errorf("%s is not suspended", uuid)
		}
	}
	if r.metrics.suspended != 2 {
		t.Errorf("suspended gauge = %d, want 2", r.metrics.suspended)
	}
	r.clock.advance(10*time.Second + 4*time.Second)
	if !r.mapper.isSuspended(layers.DMLinearName(volA)) {
		t.Fatal("the watchdog fired before the deadline plus its grace")
	}
	r.clock.advance(time.Second)
	if r.mapper.isSuspended(layers.DMLinearName(volA)) {
		t.Fatal("the watchdog did not fire at the deadline plus 5 seconds")
	}
}

// U-27
func TestSuspendRepeatedWithTheSameDeadlineSuspendsOnce(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	deadline := r.deadline(10 * time.Second)

	for range 2 {
		if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, deadline); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.mapper.mutations(); len(got) != 1 {
		t.Fatalf("mutations = %v, want one suspend", got)
	}
	if r.clock.armed() != 1 {
		t.Fatalf("armed timers = %d, want one", r.clock.armed())
	}
}

// U-28
func TestSuspendWithADeadlineAlreadyPassedSuspendsNothing(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)

	_, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(0))
	if !errors.Is(err, ErrDeadlinePassed) {
		t.Fatalf("err = %v, want ErrDeadlinePassed", err)
	}
	if len(r.mapper.mutations()) != 0 {
		t.Fatalf("a mapping was suspended: %v", r.mapper.mutations())
	}
}

// U-29
func TestSuspendOfAVolumeThatCannotSwapSuspendsNoneOfThem(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	r.stage(volB, "259:2", false)

	_, err := r.swapper.Suspend(ctx, []string{handle(volA), handle(volB)}, r.deadline(10*time.Second))
	if !errors.Is(err, ErrNotSwappable) {
		t.Fatalf("err = %v, want ErrNotSwappable", err)
	}
	if len(r.mapper.mutations()) != 0 {
		t.Fatalf("a mapping was suspended: %v", r.mapper.mutations())
	}
}

// moveNamespace is what the control plane's move looks like from the host: the
// namespace leaves the source subsystem and shows up under the target at a new
// device and namespace id.
func (r *rig) moveNamespace(uuid, dev string, nsid uint32) {
	r.devices.remove(sourceNQN)
	r.devices.add(targetNQN, uuid, dev, nsid)
}

// U-30
func TestResumeRepointsTheMappingAtTheTargetAndRecordsTheNewSubsystem(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	r.moveNamespace(volA, "259:7", 3)

	got, err := r.swapper.Resume(ctx, []string{handle(volA)}, targetNQN)
	if err != nil {
		t.Fatal(err)
	}
	name := layers.DMLinearName(volA)
	if r.mapper.isSuspended(name) || r.mapper.deviceOf(name) != "259:7" {
		t.Fatalf("mapping: suspended %t, device %s; want resumed on 259:7", r.mapper.isSuspended(name), r.mapper.deviceOf(name))
	}
	if f := r.fabricOf(volA); f.NQN != targetNQN || f.NSID != 3 || f.SecretRef != "ns/secret" || f.HostNQN != "nqn.host" {
		t.Fatalf("fabric record = %+v, want the target at nsid 3 with the rest kept", f)
	}
	if got[0].NQN != targetNQN || got[0].Suspended {
		t.Fatalf("Resume answered %+v", got[0])
	}
	if r.clock.armed() != 0 {
		t.Fatal("the watchdog is still armed after the resume")
	}
	if r.metrics.suspended != 0 {
		t.Fatalf("suspended gauge = %d after the resume", r.metrics.suspended)
	}
}

// U-31
func TestResumeRescansOnceWhenTheNamespaceHasNotAppeared(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	r.devices.remove(sourceNQN)
	r.devices.onRescan = func(nqn string) {
		if nqn == targetNQN {
			r.devices.add(targetNQN, volA, "259:7", 3)
		}
	}

	if _, err := r.swapper.Resume(ctx, []string{handle(volA)}, targetNQN); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.devices.rescans, []string{targetNQN}) {
		t.Fatalf("rescans = %v, want one of the target", r.devices.rescans)
	}
	if r.mapper.deviceOf(layers.DMLinearName(volA)) != "259:7" {
		t.Fatal("the mapping was not re-pointed after the rescan found the namespace")
	}
	if r.metrics.rescans != 1 {
		t.Fatalf("rescan observations = %d, want 1", r.metrics.rescans)
	}
}

// Regression: 2026-11-02-cg-flip-stale-source-path — after a live migration the
// host kept a controller to the old node's path of the volume's source
// subsystem, stuck reconnecting. A controller that is not live never processes
// the removal of a namespace, so it kept the namespace's identity registered,
// and the kernel ignored the same namespace in the target subsystem ("ignoring
// nsid 2 because of duplicate IDs"). The swap waited out its deadline and
// rolled back.
func TestResumeDropsTheSourcesDeadPathsSoTheNamespaceCanAppearInTheTarget(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	r.devices.remove(sourceNQN)
	r.devices.stale[sourceNQN] = 1
	// The kernel admits the namespace in the target only once no controller of
	// the source still holds its identity.
	r.devices.onRescan = func(nqn string) {
		if nqn == targetNQN && !r.devices.isStale(sourceNQN) {
			r.devices.add(targetNQN, volA, "259:7", 2)
		}
	}

	if _, err := r.swapper.Resume(ctx, []string{handle(volA)}, targetNQN); err != nil {
		t.Fatalf("resume: %v (calls %v)", err, r.devices.calls)
	}
	if !slices.Equal(r.devices.calls, []string{"drop " + sourceNQN, "rescan " + targetNQN}) {
		t.Fatalf("calls = %v, want the source's dead paths dropped before the target is rescanned", r.devices.calls)
	}
	if r.mapper.deviceOf(layers.DMLinearName(volA)) != "259:7" {
		t.Fatal("the mapping was not re-pointed at the namespace in the target")
	}
}

// A resume back onto the source (the rollback) must not drop the source's
// paths: those are the paths it is about to use.
func TestResumeOnTheSourceDropsNothing(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	r.devices.remove(sourceNQN)
	r.devices.stale[sourceNQN] = 1
	r.devices.onRescan = func(nqn string) {
		if nqn == sourceNQN {
			r.devices.add(sourceNQN, volA, "259:9", 1)
		}
	}

	if _, err := r.swapper.Resume(ctx, []string{handle(volA)}, sourceNQN); err != nil {
		t.Fatal(err)
	}
	if len(r.devices.drops) != 0 {
		t.Fatalf("drops = %v, want none on a resume back onto the source", r.devices.drops)
	}
}

// U-32
func TestResumeWhoseNamespaceNeverAppearsFailsAtTheDeadlineAndStaysSuspended(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	r.devices.remove(sourceNQN)

	_, err := r.swapper.Resume(ctx, []string{handle(volA)}, targetNQN)
	if !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("err = %v, want ErrNamespaceNotFound", err)
	}
	if !r.mapper.isSuspended(layers.DMLinearName(volA)) {
		t.Fatal("a resume that found nothing released the I/O onto a device that is gone")
	}
}

// U-33
func TestResumeOnTheSourceIsTheRollback(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	// The control plane removed and then restored the namespace, which comes
	// back as a new device under the source.
	r.devices.remove(sourceNQN)
	r.devices.add(sourceNQN, volA, "259:9", 1)

	if _, err := r.swapper.Resume(ctx, []string{handle(volA)}, sourceNQN); err != nil {
		t.Fatal(err)
	}
	name := layers.DMLinearName(volA)
	if r.mapper.isSuspended(name) || r.mapper.deviceOf(name) != "259:9" {
		t.Fatalf("mapping not resumed on the restored source device")
	}
	if f := r.fabricOf(volA); f.NQN != sourceNQN {
		t.Fatalf("fabric record = %+v, want the source", f)
	}
}

// U-34
func TestResumeOfAMappingAlreadyResumedOnTheRightDeviceChangesNothing(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)

	if _, err := r.swapper.Resume(ctx, []string{handle(volA)}, sourceNQN); err != nil {
		t.Fatal(err)
	}
	if got := r.mapper.mutations(); len(got) != 0 {
		t.Fatalf("mutations = %v, want none", got)
	}
}

// U-35
func TestResumeIgnoresTheSameNamespaceUnderAnotherSubsystem(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	if _, err := r.swapper.Suspend(ctx, []string{handle(volA)}, r.deadline(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	r.devices.remove(sourceNQN)
	r.devices.add(otherNQN, volA, "259:5", 1)

	if _, err := r.swapper.Resume(ctx, []string{handle(volA)}, targetNQN); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("err = %v, want ErrNamespaceNotFound", err)
	}
	if r.mapper.deviceOf(layers.DMLinearName(volA)) == "259:5" {
		t.Fatal("re-pointed at the namespace under a subsystem nobody asked for")
	}
}

// U-36
func TestReleaseDisconnectsASubsystemNoStagedVolumePointsAt(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)

	done, err := r.swapper.Release(ctx, otherNQN)
	if err != nil || !done {
		t.Fatalf("Release = %t, %v", done, err)
	}
	if !slices.Equal(r.connector.disconnected, []string{otherNQN}) {
		t.Fatalf("disconnected = %v", r.connector.disconnected)
	}
}

// U-37
func TestReleaseKeepsASubsystemAStagedVolumeStillPointsAt(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)

	done, err := r.swapper.Release(ctx, sourceNQN)
	if err != nil || done {
		t.Fatalf("Release = %t, %v; want kept", done, err)
	}
	if len(r.connector.disconnected) != 0 {
		t.Fatalf("disconnected %v under a staged volume", r.connector.disconnected)
	}
}
