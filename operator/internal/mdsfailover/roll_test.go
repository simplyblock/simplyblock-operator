// Tests of the planned cases: the rollout of a template change, a manual
// switchover, and the switchover a scale-down needs: O-27 to O-29, O-41, and
// O-42 (design-pnfs-mds-standby.md §7.4, §4.1).

package mdsfailover

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// settledSim is a served cluster of n guests whose active is mds-0, on revision
// rev1, with a new revision rev2 to roll to.
func settledSim(t *testing.T, replicas int) *sim {
	s := newSim(t)
	s.obs.Guests = nil
	for i := range replicas {
		g := s.newGuest(i, string(rune('a'+i)))
		s.obs.Guests = append(s.obs.Guests, g)
		s.roles[g.Pod] = nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby}
	}
	s.obs.Replicas = replicas
	s.serving(pod0, 1)
	s.bind(2)
	s.obs.UpdateRevision = rev2
	return s
}

// replace is the StatefulSet starting a new pod of the same name on rev2, which
// reports the standby role and is Ready.
func (s *sim) replace(pod string, ready bool) {
	g := s.guest(pod)
	g.Deleting = false
	g.Revision = rev2
	g.Instance = g.UID + "/new"
	g.Ready, g.HasSession = ready, ready
	s.roles[pod] = nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby}
}

func TestO27ATemplateChangeDeletesTheStandbyBeforeTheActive(t *testing.T) {
	s := settledSim(t, 2)

	s.tick()
	if !slices.Equal(s.deleted, []string{pod1}) {
		t.Fatalf("deleted = %v, want the standby first", s.deleted)
	}
	s.guest(pod1).HasSession, s.guest(pod1).Ready = false, false // being replaced
	s.tick()
	if len(s.deleted) != 1 {
		t.Fatalf("deleted = %v, want the active kept while the standby is being replaced", s.deleted)
	}

	s.replace(pod1, false)
	s.tick()
	if len(s.deleted) != 1 {
		t.Fatalf("deleted = %v, want the active kept until the replacement is Ready", s.deleted)
	}

	s.replace(pod1, true)
	s.tick()
	if !slices.Equal(s.deleted, []string{pod1, pod0}) {
		t.Errorf("deleted = %v, want the active last", s.deleted)
	}
}

func TestO42TheStandbysAreRolledOneAtATimeAndTheActiveLast(t *testing.T) {
	s := settledSim(t, 3)

	s.tick()
	if !slices.Equal(s.deleted, []string{pod2}) {
		t.Fatalf("deleted = %v, want one standby", s.deleted)
	}
	s.tick() // mds-2 is still being deleted
	if len(s.deleted) != 1 {
		t.Fatalf("deleted = %v, want no second pod while one is going away", s.deleted)
	}
	s.replace(pod2, false)
	s.tick()
	if len(s.deleted) != 1 {
		t.Fatalf("deleted = %v, want the next standby only after the replacement reports Standby", s.deleted)
	}
	s.replace(pod2, true)
	s.tick()
	if !slices.Equal(s.deleted, []string{pod2, pod1}) {
		t.Fatalf("deleted = %v, want the second standby next", s.deleted)
	}
	s.replace(pod1, true)
	s.tick()
	if !slices.Equal(s.deleted, []string{pod2, pod1, pod0}) {
		t.Errorf("deleted = %v, want the active last", s.deleted)
	}
}

func TestARolloutWithNoStandbyRestartsTheActiveInPlace(t *testing.T) {
	s := settledSim(t, 1)
	s.tick()
	if !slices.Equal(s.deleted, []string{pod0}) {
		t.Errorf("deleted = %v, want the only guest", s.deleted)
	}
}

func TestACurrentTemplateRollsNothing(t *testing.T) {
	s := settledSim(t, 2)
	s.obs.UpdateRevision = rev1
	s.tick()
	if len(s.deleted) != 0 {
		t.Errorf("deleted = %v, want none", s.deleted)
	}
}

func TestARolloutWaitsWhileATakeoverIsPending(t *testing.T) {
	s := lostActiveSim(t, 1)
	s.obs.UpdateRevision = rev2
	s.tick()
	if len(s.deleted) != 0 {
		t.Errorf("deleted = %v, want no rollout during a loss", s.deleted)
	}
}

func TestO29ARaisedStandbyCountScalesUpAndPromotesNothing(t *testing.T) {
	s := newSim(t)
	s.obs.Guests = s.obs.Guests[:1]
	s.obs.Replicas = 1
	s.serving(pod0, 1)
	s.tick()

	s.obs.Guests = append(s.obs.Guests, s.newGuest(1, "b"))
	s.obs.Replicas = 2
	s.tick()
	s.tick()
	if len(s.writes) != 0 || len(s.promotes) != 0 {
		t.Errorf("writes=%v promotes=%d, want the new pod left a standby", s.writes, len(s.promotes))
	}
}

func TestASwitchoverDeletesTheActiveAndPromotesTheNamedStandby(t *testing.T) {
	s := settledSim(t, 3)
	s.obs.UpdateRevision = rev1
	s.tick()

	if err := s.failover.Switchover(ctxBackground, pod2); err != nil {
		t.Fatalf("Switchover: %v", err)
	}
	if !slices.Equal(s.deleted, []string{pod0}) {
		t.Fatalf("deleted = %v, want the active", s.deleted)
	}
	runUntilQuiet(t, s, 6)
	if s.obs.Record.Active != pod2 || s.obs.Record.Epoch != 2 {
		t.Errorf("record = %+v, want the named standby mds-2 at epoch 2", s.obs.Record)
	}
}

func TestASwitchoverRefusesATargetThatIsNotACandidate(t *testing.T) {
	s := settledSim(t, 3)
	s.obs.UpdateRevision = rev1
	s.guest(pod2).Ready = false
	s.tick()

	for _, target := range []string{pod2, pod0, "mds-9"} {
		if err := s.failover.Switchover(ctxBackground, target); err == nil {
			t.Errorf("Switchover(%s) succeeded, want a refusal", target)
		}
	}
	if len(s.deleted) != 0 {
		t.Errorf("deleted = %v, want a refused switchover to delete nothing", s.deleted)
	}
}

func TestASwitchoverWithNoTargetLeavesTheChoiceToThePass(t *testing.T) {
	s := settledSim(t, 3)
	s.obs.UpdateRevision = rev1
	s.tick()
	if err := s.failover.Switchover(ctxBackground, ""); err != nil {
		t.Fatalf("Switchover: %v", err)
	}
	runUntilQuiet(t, s, 6)
	if s.obs.Record.Active != pod1 {
		t.Errorf("active = %q, want the lowest ordinal", s.obs.Record.Active)
	}
}

func TestO28LoweringStandbysSwitchesOverBeforeTheScaleDown(t *testing.T) {
	s := settledSim(t, 2)
	s.obs.UpdateRevision = rev1
	s.serving(pod1, 3)
	s.tick()

	ok, err := s.failover.EnsureActiveBelow(ctxBackground, 1)
	if err != nil || ok {
		t.Fatalf("EnsureActiveBelow = %v, %v, want not yet: the active is on ordinal 1", ok, err)
	}
	if !slices.Equal(s.deleted, []string{pod1}) {
		t.Fatalf("deleted = %v, want the active switched over", s.deleted)
	}
	ok, _ = s.failover.EnsureActiveBelow(ctxBackground, 1)
	if ok {
		t.Error("EnsureActiveBelow = true while the switchover is under way")
	}
	if len(s.deleted) != 1 {
		t.Errorf("deleted = %v, want one switchover", s.deleted)
	}

	runUntilQuiet(t, s, 6)
	ok, err = s.failover.EnsureActiveBelow(ctxBackground, 1)
	if err != nil || !ok {
		t.Errorf("EnsureActiveBelow after the takeover = %v, %v, want true", ok, err)
	}
	if s.obs.Record.Active != pod0 {
		t.Errorf("active = %q, want ordinal 0", s.obs.Record.Active)
	}
}

func TestO41LoweringFromTwoStandbysToOneChoosesARemainingOrdinal(t *testing.T) {
	s := settledSim(t, 3)
	s.obs.UpdateRevision = rev1
	s.serving(pod2, 3)
	s.tick()

	ok, err := s.failover.EnsureActiveBelow(ctxBackground, 2)
	if err != nil || ok {
		t.Fatalf("EnsureActiveBelow = %v, %v, want a switchover started", ok, err)
	}
	runUntilQuiet(t, s, 6)
	if got := Ordinal(s.obs.Record.Active); got >= 2 {
		t.Errorf("active = %q, want an ordinal below the new replica count", s.obs.Record.Active)
	}
}

func TestEnsureActiveBelowIsTrueWhenTheActiveIsAlreadyBelow(t *testing.T) {
	s := settledSim(t, 2)
	s.obs.UpdateRevision = rev1
	s.tick()
	ok, err := s.failover.EnsureActiveBelow(ctxBackground, 1)
	if err != nil || !ok || len(s.deleted) != 0 {
		t.Errorf("= %v, %v deleted=%v, want true with nothing deleted", ok, err, s.deleted)
	}
}

func TestEnsureActiveBelowReportsWhenNoStandbyRemains(t *testing.T) {
	s := settledSim(t, 2)
	s.obs.UpdateRevision = rev1
	s.serving(pod1, 3)
	s.guest(pod0).Ready = false
	s.tick()
	ok, err := s.failover.EnsureActiveBelow(ctxBackground, 1)
	if ok || !errors.Is(err, ErrNoRemainingStandby) {
		t.Errorf("= %v, %v, want not ready with ErrNoRemainingStandby", ok, err)
	}
	if len(s.deleted) != 0 {
		t.Errorf("deleted = %v, want the active kept when it has nowhere to go", s.deleted)
	}
}

func TestAClockBoundDetectionWindowIsConfigurable(t *testing.T) {
	s := lostActiveSim(t, 1)
	s.config.DetectionWindow = 2 * time.Second
	s.tick()
	s.clock.advance(3 * time.Second)
	s.tick()
	if len(s.writes) != 1 {
		t.Errorf("writes = %v, want a takeover after a 2s window", s.writes)
	}
}
