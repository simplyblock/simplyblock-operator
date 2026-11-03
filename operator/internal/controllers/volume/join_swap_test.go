// The JoinConsistencyGroup swap: the consuming hosts, the suspend window, the
// move, the resume, the commit, and every rollback out of the window
// (design-cg-live-subsystem-flip.md §6, §7, §11).

package volume

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/volstack/swaprpc"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func (w *joinWorld) node(name string) *fakeSwapNode { return w.peers.nodes[name] }

// U-72
func TestTheConsumingHostsAreTheNodesOfTheVolumeAttachments(t *testing.T) {
	w := newJoinWorld(t)

	ops := w.runUntil(stepSuspending, 25)
	hosts := map[string][]string{}
	for _, h := range ops.Status.JoinConsistencyGroup.Hosts {
		hosts[h.Node] = h.PersistentVolumes
	}
	if len(hosts) != 2 || !slices.Equal(hosts[joinHost], []string{testPVName}) ||
		!slices.Equal(hosts[memberHost], []string{memberPVName}) {
		t.Errorf("hosts = %v, want %s with the joiner and %s with the member", hosts, joinHost, memberHost)
	}
	for _, name := range []string{joinHost, memberHost} {
		if w.log.index("prepare "+name+" "+joinTargetNQN+" 2") < 0 {
			t.Errorf("%s was not given both paths to the target", name)
		}
	}
}

// Regression: 2026-11-02-cg-flip-prepare-without-host — the operator asked the
// control plane to prepare the target without naming a host, the control plane
// answered with no connect entries, and every host's Prepare was refused for
// having no path, so a live flip never got past AttachingPaths.
func TestEachHostIsPreparedWithItsOwnHostNQNAndGetsTheTargetPaths(t *testing.T) {
	w := newJoinWorld(t)

	ops := w.runUntil(stepSuspending, 25)
	if ops.Status.Step.State != string(stepSuspending) {
		t.Fatalf("step = %q (%s), want Suspending", ops.Status.Step.State, ops.Status.Message)
	}
	for _, name := range []string{joinHost, memberHost} {
		if !slices.Contains(w.api.prepareHosts, hostNQNOf(name)) {
			t.Errorf("no prepare named %s's host NQN %s; prepares named %q", name, hostNQNOf(name), w.api.prepareHosts)
		}
		if w.log.index("prepare "+name+" "+joinTargetNQN+" 2") < 0 {
			t.Errorf("%s was not given both paths to the target: %v", name, w.log.calls)
		}
	}
}

// U-68, U-83, U-86, U-87
func TestAFlipSuspendsEveryHostBeforeMovingAndResumesThemOnTheTarget(t *testing.T) {
	w := newJoinWorld(t)

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}

	move := w.log.index("move")
	if move < 0 {
		t.Fatal("the namespaces were never moved")
	}
	for _, name := range []string{joinHost, memberHost} {
		if s := w.log.index("suspend " + name); s < 0 || s > move {
			t.Errorf("%s was suspended at %d, the move ran at %d", name, s, move)
		}
		if r := w.log.index("resume " + name + " " + joinTargetNQN); r < move {
			t.Errorf("%s was resumed on the target at %d, before the move at %d", name, r, move)
		}
	}
	if w.log.count("move") != 1 || w.log.count("commit") != 1 {
		t.Errorf("moves = %d, commits = %d, want 1 each", w.log.count("move"), w.log.count("commit"))
	}
	if w.log.index("commit") < w.log.last("resume ") {
		t.Error("the move was committed before every host resumed")
	}
	if w.log.index("release "+joinHost+" "+joinSourceNQN) < 0 ||
		w.log.index("release "+memberHost+" "+memberNQN) < 0 {
		t.Errorf("the sources were not released: %v", w.log.calls)
	}
	if w.node(joinHost).pointsAt[handleOf(testVolumeID)] != joinTargetNQN {
		t.Error("the joiner's host does not point at the target")
	}
	if ops.Status.JoinConsistencyGroup.NamespacesAt != simplyblockv1alpha2.SubsystemSideTarget {
		t.Errorf("namespacesAt = %q, want Target", ops.Status.JoinConsistencyGroup.NamespacesAt)
	}
	events := w.events()
	if !hasEvent(events, ReasonConsistencyGroupColocated) || !hasEvent(events, ReasonSubsystemSwapCompleted) {
		t.Errorf("events = %v, want ConsistencyGroupColocated and SubsystemSwapCompleted", events)
	}
}

// U-74
func TestAHostWithoutTheSwapCapabilityFailsBeforeAnythingIsSuspended(t *testing.T) {
	w := newJoinWorld(t)
	w.peers.incapable[memberHost] = true

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if n := w.log.count("suspend") + w.log.count("move"); n != 0 {
		t.Errorf("suspended or moved %d times with a host that cannot swap", n)
	}
	if w.log.count("rollback") == 0 {
		t.Error("the reserved move was not rolled back")
	}
	if e := eventWith(w.events(), ReasonSubsystemSwapUnavailable); !strings.Contains(e, memberHost) {
		t.Errorf("SubsystemSwapUnavailable event = %q, want it to name %s", e, memberHost)
	}
}

// U-74, a host with no link at all
func TestAHostWithNoLinkFailsBeforeAnythingIsSuspended(t *testing.T) {
	w := newJoinWorld(t)
	delete(w.peers.nodes, memberHost)

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("suspend") != 0 {
		t.Error("a host was suspended")
	}
}

// U-75
func TestAVolumeThatCannotSwapFailsNamingItsClaim(t *testing.T) {
	w := newJoinWorld(t)
	w.node(memberHost).notSwappable[handleOf(memberVolumeID)] = true

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("suspend") != 0 {
		t.Error("a host was suspended")
	}
	if e := eventWith(w.events(), ReasonSubsystemSwapUnavailable); !strings.Contains(e, "app/member") {
		t.Errorf("SubsystemSwapUnavailable event = %q, want it to name claim app/member", e)
	}
}

// U-76
func TestAMoveWithNoAttachedVolumeSkipsTheNodes(t *testing.T) {
	w := newJoinWorld(t)
	for _, pv := range []string{testPVName, memberPVName} {
		va := attachment(pv, "")
		if err := w.r.Delete(context.Background(), va); err != nil {
			t.Fatal(err)
		}
	}

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("move") != 1 {
		t.Errorf("moves = %d, want 1", w.log.count("move"))
	}
	for _, prefix := range []string{"describe", "prepare ", "suspend", "resume", "release"} {
		if n := w.log.count(prefix); n != 0 {
			t.Errorf("%s called %d times with no consuming host", prefix, n)
		}
	}
}

// U-77
func TestTheSuspendDeadlineIsPersistedBeforeTheFirstSuspend(t *testing.T) {
	w := newJoinWorld(t)
	want := w.now.Add(10 * time.Second)

	w.runUntil(stepSuspending, 25)
	w.run(1)
	if w.log.count("suspend") == 0 {
		w.run(1)
	}
	got := w.ops().Status.JoinConsistencyGroup.SuspendDeadline
	if got == nil || !got.Time.Equal(want) {
		t.Fatalf("suspendDeadline = %v, want %v", got, want)
	}
	if d := w.node(joinHost).deadline; !d.Equal(want) {
		t.Errorf("the host was given %v, want the persisted %v", d, want)
	}
	w.run(5)
	if len(w.api.deadlines) == 0 || !w.api.deadlines[0].Equal(want) {
		t.Errorf("the move was given %v, want %v", w.api.deadlines, want)
	}
}

// rolledBack asserts the rollback's common outcome: the move taken back, no
// host left on the target, and the operation failed with the event.
func (w *joinWorld) rolledBack(ops *simplyblockv1alpha2.PersistentVolumeOps) {
	w.t.Helper()
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseFailed {
		w.t.Fatalf("phase = %q (%s), want Failed", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("rollback") == 0 {
		w.t.Error("the move was not rolled back")
	}
	if w.log.count("commit") != 0 {
		w.t.Error("a rolled-back move was committed")
	}
	if ops.Status.JoinConsistencyGroup.RollbackReason == "" {
		w.t.Error("status.rollbackReason is empty")
	}
	if !hasEvent(w.events(), ReasonSubsystemSwapRolledBack) {
		w.t.Error("no SubsystemSwapRolledBack event")
	}
}

// U-78
func TestOneHostFailingToSuspendRollsBackWithoutMoving(t *testing.T) {
	w := newJoinWorld(t)
	w.node(memberHost).suspendErr = fmt.Errorf("dmsetup suspend: %w", swaprpc.ErrNotSwappable)

	ops := w.run(40)
	w.rolledBack(ops)
	if w.log.count("move") != 0 {
		t.Error("the namespaces were moved although a host was not suspended")
	}
	if w.node(joinHost).suspended[handleOf(testVolumeID)] {
		t.Error("the host that suspended was never resumed")
	}
	if w.log.index("resume "+joinHost+" "+joinSourceNQN) < 0 {
		t.Errorf("the joiner's host was not resumed on its source: %v", w.log.calls)
	}
	if w.log.index("release "+joinHost+" "+joinTargetNQN) < 0 {
		t.Errorf("the target was not released after the rollback: %v", w.log.calls)
	}
}

// U-79
func TestAHostThatRestartedBeforeTheMoveRollsBack(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepMovingNamespaces, 30)
	w.node(memberHost).instance = "instance-restarted"
	w.node(memberHost).suspended = map[string]bool{}

	w.rolledBack(w.run(40))
	if w.log.count("move") != 0 {
		t.Error("the namespaces were moved under a restarted host")
	}
}

// U-80
func TestAHostNoLongerSuspendedBeforeTheMoveRollsBack(t *testing.T) {
	w := newJoinWorld(t)

	w.runUntil(stepMovingNamespaces, 30)
	w.node(joinHost).suspended[handleOf(testVolumeID)] = false

	w.rolledBack(w.run(40))
	if w.log.count("move") != 0 {
		t.Error("the namespaces were moved under a resumed host")
	}
}

// U-81
func TestAMoveRefusedForItsDeadlineRollsBack(t *testing.T) {
	w := newJoinWorld(t)
	w.api.startErr = fmt.Errorf("move: %w", controlplane.ErrDeadlinePassed)

	ops := w.run(40)
	w.rolledBack(ops)
	if w.log.index("resume "+memberHost+" "+memberNQN) < 0 {
		t.Errorf("the member's host was not resumed on its source: %v", w.log.calls)
	}
}

// U-82
func TestAPartialMoveResumesEveryHostOnTheSource(t *testing.T) {
	w := newJoinWorld(t)
	w.api.startErr = fmt.Errorf("move: %w", controlplane.ErrMoveFailed)

	ops := w.run(40)
	w.rolledBack(ops)
	if ops.Status.JoinConsistencyGroup.NamespacesAt != simplyblockv1alpha2.SubsystemSideSource {
		t.Errorf("namespacesAt = %q, want Source", ops.Status.JoinConsistencyGroup.NamespacesAt)
	}
	if w.log.count("resume "+joinHost+" "+joinTargetNQN)+w.log.count("resume "+memberHost+" "+joinTargetNQN) != 0 {
		t.Error("a host was resumed on the target after a failed move")
	}
}

// A control plane that is briefly unreachable is retried inside the window.
func TestATransientMoveFailureIsRetriedInsideTheWindow(t *testing.T) {
	w := newJoinWorld(t)
	w.api.startErr = statusErr

	w.runUntil(stepMovingNamespaces, 30)
	w.run(2)
	w.api.startErr = nil
	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
}

// The window closing on an unreachable control plane rolls back.
func TestAMoveStillFailingAtTheDeadlineRollsBack(t *testing.T) {
	w := newJoinWorld(t)
	w.api.startErr = statusErr

	w.runUntil(stepMovingNamespaces, 30)
	w.now = w.now.Add(11 * time.Second)
	w.rolledBack(w.run(40))
}

// U-84
func TestAHostThatCannotFindTheNamespaceRollsBackToTheSource(t *testing.T) {
	w := newJoinWorld(t)
	w.node(memberHost).resumeErrs = []error{fmt.Errorf("resume: %w", swaprpc.ErrNamespaceNotFound)}

	ops := w.run(40)
	w.rolledBack(ops)
	if w.log.index("resume "+memberHost+" "+memberNQN) < 0 {
		t.Errorf("the member's host was not resumed on its source: %v", w.log.calls)
	}
	if w.log.index("resume "+joinHost+" "+joinSourceNQN) < 0 {
		t.Errorf("the joiner's host, already on the target, was not taken back to its source: %v", w.log.calls)
	}
}

// U-85
func TestACommitFailureIsRetriedWithoutSuspendingAgain(t *testing.T) {
	w := newJoinWorld(t)
	w.api.commitErrs = []error{statusErr, statusErr}

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	if w.log.count("commit") != 3 {
		t.Errorf("commits = %d, want two failures and a success", w.log.count("commit"))
	}
	for _, name := range []string{joinHost, memberHost} {
		if n := w.log.count("suspend " + name); n != 1 {
			t.Errorf("%s suspended %d times, want 1", name, n)
		}
	}
}

// U-134
// Regression: 2026-11-03-cg-join-member-count — a volume moved alone into the
// subsystem its group already had reported no other member in it, while the
// group's existing member was sitting there.
func TestAJoinIntoTheGroupsExistingSubsystemSaysSoAndCountsWhatMoved(t *testing.T) {
	w := newJoinWorld(t)
	w.api.move = controlplane.SubsystemMove{
		ID: joinMoveID, TargetNQN: memberNQN, NamespacesAt: controlplane.NamespacesAtSource,
		Volumes: []controlplane.MovingNamespace{
			{LvolID: testVolumeID, SourceNQN: joinSourceNQN, SourceNSID: 1, NSUUID: testVolumeID, TargetNSID: 2},
		},
	}

	ops := w.run(40)
	if ops.Status.Phase != simplyblockv1alpha2.PersistentVolumeOpsPhaseSucceeded {
		t.Fatalf("phase = %q (%s), want Succeeded", ops.Status.Phase, ops.Status.Message)
	}
	want := "1 namespace(s) moved into the group's existing subsystem " + memberNQN
	if ops.Status.Message != want {
		t.Errorf("message = %q, want %q", ops.Status.Message, want)
	}
	if got := eventWith(w.events(), ReasonConsistencyGroupColocated); !strings.Contains(got, want) {
		t.Errorf("ConsistencyGroupColocated = %q, want it to say %q", got, want)
	}
}

// U-134
func TestAWholeGroupMoveIntoAFreshSubsystemCountsEveryNamespace(t *testing.T) {
	w := newJoinWorld(t)

	ops := w.run(40)
	want := "2 namespace(s) moved into the group's new subsystem " + joinTargetNQN
	if ops.Status.Message != want {
		t.Errorf("message = %q, want %q", ops.Status.Message, want)
	}
}
