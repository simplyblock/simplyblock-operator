// The JoinConsistencyGroup of a volume that is already a member of another
// group: its claim was relabeled, so it leaves the subsystem it shares with
// that group's members, then the group, and only then joins the one its claim
// names (design-cg-live-subsystem-flip.md §4).

package volume

import (
	"fmt"
	"slices"
	"testing"

	"k8s.io/client-go/tools/events"

	"github.com/simplyblock/atlas/controlplane"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	// oldGroupID is the group the relabeled volume is a member of.
	oldGroupID = "66666666-6666-4666-8666-666666666666"
	// oldGroupNQN is the subsystem the volume shares with that group.
	oldGroupNQN = "nqn.2023-02.io.simplyblock:cluster:lvol:oldgroup"
	leaveMoveID = "leave-1"
	splitNQN    = "nqn.2023-02.io.simplyblock:cluster:lvol:split"
)

// relabeled makes the test volume a member of the old group, in the subsystem
// it shares with that group's members, and has a detach take it out.
func (w *joinWorld) relabeled() {
	w.cp.volume.GroupID = oldGroupID
	w.cp.volume.NQN = oldGroupNQN
	w.api.leave = controlplane.SubsystemMove{
		ID: leaveMoveID, TargetNQN: splitNQN, TargetIsNew: true,
		NamespacesAt: controlplane.NamespacesAtSource,
		Volumes: []controlplane.MovingNamespace{
			{LvolID: testVolumeID, SourceNQN: oldGroupNQN, SourceNSID: 2, NSUUID: testVolumeID, TargetNSID: 1},
		},
	}
	w.api.onDetach = func() { w.cp.volume.GroupID = "" }
}

// callsOn are the subsystem-move calls made on a group, a run of one call
// counted once: a move is prepared once for the target and once for each host.
func (w *joinWorld) callsOn(groupID string) []string {
	var out []string
	for _, c := range w.api.moveGroups {
		if len(c) > len(groupID) && c[len(c)-len(groupID):] == groupID {
			out = append(out, c[:len(c)-len(groupID)-1])
		}
	}
	return slices.Compact(out)
}

// Regression: 2026-11-03-cg-relabel-move. A claim relabeled from one group to
// another raised a ConsistencyGroupConflict and nothing moved: the volume stayed
// in its old group and its old group's subsystem for good.
func TestARelabeledVolumeLeavesItsGroupsSubsystemThenItsGroupThenJoinsTheNewOne(t *testing.T) {
	w := newJoinWorld(t)
	w.relabeled()

	ops := w.run(80)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}

	if got, want := w.callsOn(oldGroupID), []string{"leave", "prepare", "move", "commit"}; !slices.Equal(got, want) {
		t.Errorf("calls on the old group = %v, want %v", got, want)
	}
	if got := w.callsOn(joinGroupID); !slices.Contains(got, "create") || !slices.Contains(got, "commit") {
		t.Errorf("calls on the new group = %v, want its own move created and committed", got)
	}
	if slices.Contains(w.callsOn(joinGroupID), "leave") {
		t.Error("the leave was asked of the group the volume joins")
	}

	// The order is the property: out of the old subsystem, then out of the old
	// group, and only then planned and joined into the new one.
	commitLeave := w.log.index("commit")
	detach := w.log.index("detach " + oldGroupID)
	plan, join := w.log.index("plan"), w.log.index("join")
	if commitLeave < 0 || detach < commitLeave || plan < detach || join < plan {
		t.Errorf("order: leave commit %d, detach %d, plan %d, join %d; want each after the one before: %v",
			commitLeave, detach, plan, join, w.log.calls)
	}
	if w.api.detached != 1 {
		t.Errorf("detached %d times, want 1", w.api.detached)
	}

	if !slices.Contains(w.log.calls, "resume "+joinHost+" "+splitNQN) {
		t.Errorf("the joiner's host was never resumed on the split subsystem: %v", w.log.calls)
	}
	if w.node(joinHost).pointsAt[handleOf(testVolumeID)] != joinTargetNQN {
		t.Errorf("the joiner points at %q, want the new group's subsystem %s",
			w.node(joinHost).pointsAt[handleOf(testVolumeID)], joinTargetNQN)
	}

	leave := ops.Status.JoinConsistencyGroup.Leave
	if leave == nil || leave.GroupUUID != oldGroupID || !leave.Split || leave.SplitNQN != splitNQN || !leave.Detached {
		t.Errorf("leave = %+v, want old group %s split into %s and detached", leave, oldGroupID, splitNQN)
	}
	recorded := w.events()
	for _, reason := range []string{ReasonConsistencyGroupSplit, ReasonConsistencyGroupLeft} {
		if !hasEvent(recorded, reason) {
			t.Errorf("no %s event", reason)
		}
	}
}

// Regression: 2026-11-03-cg-relabel-move. A relabeled volume whose new group
// sits on another node is migrated there, and the Migrate operation takes the
// volume's lock, so the leave must have given back every lock its swap took.
func TestALeaveGivesBackItsLocksBeforeTheMigrationToTheNewGroup(t *testing.T) {
	w := newJoinWorld(t)
	w.relabeled()
	w.offPin()

	w.runUntil(stepJoinMigrating, 60)
	w.run(2)
	if n := len(w.children()); n != 1 {
		t.Fatalf("child migrations = %d, want 1", n)
	}
	if w.api.detached != 1 {
		t.Errorf("detached %d times before the migration, want 1", w.api.detached)
	}
	for _, name := range []string{testPVName, memberPVName} {
		if got := w.pv(name).Annotations[simplyblockv1alpha2.PersistentVolumeOpsLock]; got != "" {
			t.Errorf("volume %s still locked by %q while the migration needs it", name, got)
		}
	}
}

// Regression: 2026-11-03-cg-relabel-move. A volume alone in its subsystem has
// nothing to split out of, so it leaves its group without a swap.
func TestAVolumeAloneInItsSubsystemLeavesWithoutASwap(t *testing.T) {
	w := newJoinWorld(t)
	w.relabeled()
	w.api.leave = controlplane.SubsystemMove{}

	ops := w.run(80)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if got, want := w.callsOn(oldGroupID), []string{"leave"}; !slices.Equal(got, want) {
		t.Errorf("calls on the old group = %v, want only the leave", got)
	}
	detach := w.log.index("detach " + oldGroupID)
	if first := w.log.index("suspend"); detach < 0 || (first >= 0 && first < detach) {
		t.Errorf("a host was suspended (%d) before the detach (%d), with nothing to split", first, detach)
	}
	if leave := ops.Status.JoinConsistencyGroup.Leave; leave == nil || !leave.Split || leave.SplitNQN != "" || !leave.Detached {
		t.Errorf("leave = %+v, want split with no subsystem and detached", leave)
	}
}

// Regression: 2026-11-03-cg-relabel-move. A node that cannot hold another
// subsystem refuses the split, and the volume must stay a member of its old
// group rather than be left in no group.
func TestALeaveRefusedAtTheSubsystemLimitFailsAndKeepsTheVolumeInItsGroup(t *testing.T) {
	w := newJoinWorld(t)
	w.relabeled()
	w.api.leaveErr = fmt.Errorf("leave: %w", controlplane.ErrSubsystemLimitReached)

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("detach") != 0 || w.log.count("plan") != 0 || w.log.count("join") != 0 {
		t.Errorf("a refused leave went on: %v", w.log.calls)
	}
	if !hasEvent(w.events(), ReasonSubsystemLimitReached) {
		t.Error("no SubsystemLimitReached event")
	}
}

// Regression: 2026-11-03-cg-relabel-move. A split rolled back inside its
// window leaves the volume in its old subsystem, so it must stay a member of
// the old group too.
func TestARollbackInsideTheLeaveWindowFailsAndKeepsTheVolumeInItsGroup(t *testing.T) {
	w := newJoinWorld(t)
	w.relabeled()
	w.api.startErr = fmt.Errorf("move: %w", controlplane.ErrDeadlinePassed)

	ops := w.run(60)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if got := w.callsOn(oldGroupID); !slices.Contains(got, "rollback") {
		t.Errorf("calls on the old group = %v, want the split rolled back there", got)
	}
	if w.log.count("detach") != 0 || w.log.count("plan") != 0 {
		t.Errorf("a rolled-back leave went on: %v", w.log.calls)
	}
	if !slices.Contains(w.log.calls, "resume "+joinHost+" "+oldGroupNQN) {
		t.Errorf("the joiner's host was not resumed on the old subsystem: %v", w.log.calls)
	}
}

// Regression: 2026-11-03-cg-relabel-move. An operator restarted between the
// split and the detach resumes from status: one leave, one detach, and a
// backend still naming the old group does not send the volume back to Leaving.
func TestARestartBetweenTheSplitAndTheDetachLeavesOnce(t *testing.T) {
	w := newJoinWorld(t)
	w.relabeled()
	w.api.onDetach = nil // the backend keeps reporting the old group
	w.api.detachErrs = []error{statusErr, statusErr}

	shared := w.r
	for range 80 {
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
	if n := w.log.count("leave"); n != 1 {
		t.Errorf("leaves = %d, want 1", n)
	}
	if w.api.detached != 1 {
		t.Errorf("detached %d times, want 1", w.api.detached)
	}
	if got, want := w.callsOn(oldGroupID), []string{"leave", "prepare", "move", "commit"}; !slices.Equal(got, want) {
		t.Errorf("calls on the old group = %v, want %v", got, want)
	}
	if n := w.log.count("move"); n != 2 {
		t.Errorf("namespace moves = %d, want one split and one join", n)
	}
}

// Regression: 2026-11-03-cg-relabel-move. A volume already in the group its
// claim names has nothing to leave.
func TestAVolumeAlreadyInTheNamedGroupDoesNotLeaveIt(t *testing.T) {
	w := newJoinWorld(t)
	w.cp.volume.GroupID = joinGroupID

	ops := w.run(60)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("leave") != 0 || w.log.count("detach") != 0 || ops.Status.JoinConsistencyGroup.Leave != nil {
		t.Errorf("a member of the named group left it: %v, leave = %+v", w.log.calls, ops.Status.JoinConsistencyGroup.Leave)
	}
}
