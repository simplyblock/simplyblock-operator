// The JoinConsistencyGroup across restarts, aborts, deletion, and the lock on
// every volume it moves (design-cg-live-subsystem-flip.md §7, §8.3).

package volume

import (
	"context"
	"testing"

	"k8s.io/client-go/tools/events"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// U-89
func TestARestartedReconcilerRepeatsNoCompletedCall(t *testing.T) {
	w := newJoinWorld(t)
	shared := w.r

	// Every pass is a fresh reconciler over the same world, which is what a
	// restart leaves: nothing but what status recorded.
	for range 40 {
		fresh := *shared
		fresh.Recorder = events.NewFakeRecorder(256)
		w.r = &fresh
		if _, err := w.pass(); err != nil {
			t.Fatal(err)
		}
		if terminal(w.ops().Status.Phase) {
			break
		}
	}
	w.r = shared

	if ops := w.ops(); ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	for prefix, want := range map[string]int{
		"join": 1, "move": 1, "commit": 1,
		"suspend " + joinHost: 1, "suspend " + memberHost: 1,
		"resume " + joinHost: 1, "resume " + memberHost: 1,
	} {
		if got := w.log.count(prefix); got != want {
			t.Errorf("%q called %d times, want %d", prefix, got, want)
		}
	}
}

// U-90
func TestAnAbortBeforeTheSuspendRollsTheMoveBackAndEndsAborted(t *testing.T) {
	w := newJoinWorld(t)

	// The member's host is briefly unreachable, so the joiner's host is given
	// the target's paths and the step waits on the other.
	w.peers.unreachable[memberHost] = true
	w.runUntil(stepJoinAttachingPaths, 25)
	w.run(2)
	w.update(func(ops *simplyblockv1alpha2.PersistentVolumeOps) { ops.Spec.Abort = true })
	ops := w.run(20)

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseAborted {
		t.Fatalf("phase = %q (%s), want Aborted", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("rollback") == 0 {
		t.Error("the reserved move was not rolled back")
	}
	if n := w.log.count("suspend") + w.log.count("move"); n != 0 {
		t.Errorf("suspended or moved %d times after the abort", n)
	}
	if w.log.index("prepare "+joinHost) < 0 {
		t.Fatal("the joiner's host was never given the target's paths, so the release is untested")
	}
	if w.log.index("release "+joinHost+" "+joinTargetNQN) < 0 {
		t.Errorf("%s holds paths to the target that were never released: %v", joinHost, w.log.calls)
	}
}

// U-91
func TestAnAbortDuringTheWindowIsNotHonored(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepSuspending, 30)
	w.update(func(ops *simplyblockv1alpha2.PersistentVolumeOps) { ops.Spec.Abort = true })
	ops := w.run(40)

	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want the flip to finish", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("rollback") != 0 {
		t.Error("an abort inside the window rolled the move back")
	}
}

// U-92
func TestDeletingAJoinWhoseNamespacesMovedCommits(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepJoinResuming, 35)
	w.run(1)
	if err := w.r.Delete(context.Background(), w.ops()); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := w.pass(); err != nil {
			t.Fatal(err)
		}
	}
	if w.log.count("commit") == 0 {
		t.Error("the moved namespaces were not committed before the operation went")
	}
	if w.log.count("rollback") != 0 {
		t.Error("moved namespaces were rolled back")
	}
}

// U-93
func TestDeletingAJoinBeforeItsNamespacesMovedRollsBack(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepJoinAttachingPaths, 25)
	if err := w.r.Delete(context.Background(), w.ops()); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := w.pass(); err != nil {
			t.Fatal(err)
		}
	}
	if w.log.count("rollback") == 0 {
		t.Error("the reserved move was not rolled back")
	}
	if w.log.count("commit") != 0 {
		t.Error("an unmoved group was committed")
	}
}

// U-94
func TestTheLockCoversEverySourceAndTargetVolume(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepSuspending, 30)
	for _, name := range []string{testPVName, memberPVName} {
		if got := w.pv(name).Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]; got != joinOpsName {
			t.Errorf("volume %s lock = %q, want %s", name, got, joinOpsName)
		}
	}
}

// U-95
func TestAVolumeHeldByAnotherOperationDefersTheSwap(t *testing.T) {
	other := testOperation()
	other.Name = "other-move"
	other.Status.Phase = simplyblockv1alpha2.PersistentVolumeOpsPhaseRunning
	w := newJoinWorld(t, other)
	held := w.pv(memberPVName)
	held.Annotations = map[string]string{simplyblockv1alpha2.PersistentVolumeOpsLock: "other-move"}
	if err := w.r.Update(context.Background(), held); err != nil {
		t.Fatal(err)
	}

	w.runUntil(stepSelectingSubsystem, 15)
	w.run(4)

	ops := w.ops()
	if ops.Status.Step.State != string(stepSelectingSubsystem) || ops.Status.DeferredSince == nil {
		t.Errorf("step = %q, deferredSince = %v, want held at selection", ops.Status.Step.State, ops.Status.DeferredSince)
	}
	if !hasEvent(w.events(), ReasonSubsystemSwapDeferred) {
		t.Error("no SubsystemSwapDeferred event")
	}
	if got := w.pv(testPVName).Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]; got != "" {
		t.Errorf("the joiner holds %q while the swap waits; a waiting operation holds nothing", got)
	}
	if w.log.count("prepare") != 0 {
		t.Error("the target was prepared while the swap waited")
	}
}

// U-96
func TestEveryTerminalPathReleasesOnlyItsOwnLocks(t *testing.T) {
	w := newJoinWorld(t)
	w.node(memberHost).suspendErr = errNotReady

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q, want Failed", ops.Status.Phase)
	}
	for _, name := range []string{testPVName, memberPVName} {
		if got := w.pv(name).Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]; got != "" {
			t.Errorf("volume %s still locked by %q", name, got)
		}
	}
}

// U-97
func TestATerminalJoinIssuesNoCall(t *testing.T) {
	w := newJoinWorld(t)
	w.run(40)
	calls := len(w.log.calls)

	w.run(3)
	if len(w.log.calls) != calls {
		t.Errorf("a finished join made %d more calls: %v", len(w.log.calls)-calls, w.log.calls[calls:])
	}
}

var errNotReady = errorString("not ready")

type errorString string

func (e errorString) Error() string { return string(e) }
