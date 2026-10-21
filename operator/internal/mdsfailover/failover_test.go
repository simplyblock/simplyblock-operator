// Tests of the takeover as the operator drives it: O-07 to O-09, O-11, O-12,
// O-14 to O-19, O-22 to O-26, O-30, O-31, O-37, O-39, O-44, and O-45, against
// the simulated cluster of sim_test.go (design-pnfs-mds-standby.md §7).

package mdsfailover

import (
	"slices"
	"strings"
	"testing"
	"time"

	atlaslvol "github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// runUntilQuiet runs passes, assembling the exports between them, until a pass
// asks for nothing more, and returns how many it took.
func runUntilQuiet(t *testing.T, s *sim, limit int) {
	t.Helper()
	for i := 1; i <= limit; i++ {
		res, err := s.pass()
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		s.assemble()
		s.clock.advance(time.Second)
		if res.Settled {
			return
		}
	}
	t.Fatalf("still running after %d passes; log=%v", limit, s.log)
}

func firstExportSim(t *testing.T) *sim {
	s := newSim(t)
	s.unbound(1)
	return s
}

// lostActiveSim is a cluster whose active mds-0 serves n exports and is lost.
func lostActiveSim(t *testing.T, exports int) *sim {
	s := newSim(t)
	s.serving(pod0, 1)
	s.bind(exports)
	s.lose(pod0)
	return s
}

func TestO07TheFirstExportRecordsEpochOneAndPromotesTheStandby(t *testing.T) {
	s := firstExportSim(t)

	runUntilQuiet(t, s, 5)

	if len(s.writes) != 1 || s.writes[0].Epoch != 1 || s.writes[0].Active != pod0 {
		t.Fatalf("record writes = %+v, want one at epoch 1 naming the lowest ordinal", s.writes)
	}
	if s.writes[0].Instance != boot0 {
		t.Errorf("recorded instance = %q, want the target's boot u0/c0", s.writes[0].Instance)
	}
	if len(s.promotes) != 1 {
		t.Fatalf("promotes = %d, want 1", len(s.promotes))
	}
	req := s.promotes[0].req
	wantKey, _ := nfsexport.MDSReservationKey(simStatefulSetUID, 1)
	if req.Epoch != 1 || req.Key != wantKey || req.HostNQN != "nqn-sts-uid-0" ||
		req.StatefulSetUID != simStatefulSetUID {
		t.Errorf("promote request = %+v, want epoch 1, key %#x, the host NQN of ordinal 0", req, wantKey)
	}
	if req.StateVolume.VolumeUUID != simStateVolumeID || req.StateVolume.ClusterID != simClusterID || req.StateVolume.PoolID != "pool1" {
		t.Errorf("state volume = %+v, want the recorded handle", req.StateVolume)
	}
	if !s.hasEvent("driver", "Normal", "MDSPromoted") {
		t.Errorf("events = %v, want a Normal MDSPromoted for a first promotion", s.reasons())
	}
}

func TestAFirstPromotionWaitsForTheStateVolumeAndForAnExport(t *testing.T) {
	s := newSim(t) // no export
	s.tick()
	if len(s.writes) != 0 {
		t.Errorf("record written with no export: %+v", s.writes)
	}

	s = firstExportSim(t)
	s.obs.StateVolume = ""
	s.tick()
	if len(s.writes) != 0 || len(s.promotes) != 0 {
		t.Errorf("promoted with no state volume recorded: writes=%+v", s.writes)
	}
}

func TestO08ALostRecordWriteSendsNoPromote(t *testing.T) {
	s := firstExportSim(t)
	s.conflictNext = true

	res, err := s.pass()
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(s.promotes) != 0 || s.logged("withdraw") {
		t.Errorf("log = %v, want no withdraw and no Promote after a conflict", s.log)
	}
	if res.RequeueAfter == 0 {
		t.Error("a conflict ends the pass with no requeue")
	}
	if !s.hasEvent("driver", "Normal", "MDSPromotionConflict") {
		t.Errorf("events = %v, want MDSPromotionConflict", s.reasons())
	}
}

// frozen is an observer whose cache is a pass behind: it returns what it saw
// the first time, whatever the world has become.
type frozen struct {
	inner Observer
	obs   *Observation
}

func (f *frozen) Observe(ctx contextT) (Observation, error) {
	if f.obs == nil {
		o, err := f.inner.Observe(ctx)
		if err != nil {
			return o, err
		}
		f.obs = &o
	}
	return *f.obs, nil
}

func TestO09TwoPassesFromOneCachedVersionPromoteExactlyOnce(t *testing.T) {
	s := firstExportSim(t)
	stale := &frozen{inner: s}
	a, b := s.deps(), s.deps()
	a.Observer, b.Observer = stale, stale
	fa, fb := New(a), New(b)

	if _, err := fa.Pass(ctxBackground); err != nil {
		t.Fatalf("A: %v", err)
	}
	if _, err := fb.Pass(ctxBackground); err != nil {
		t.Fatalf("B: %v", err)
	}

	if len(s.writes) != 1 {
		t.Errorf("record writes = %+v, want exactly one winner", s.writes)
	}
	if len(s.promotes) != 1 {
		t.Errorf("promotes = %d, want exactly one: the loser must not promote", len(s.promotes))
	}
	if !s.logged("conflict") {
		t.Errorf("log = %v, want the loser's conflict", s.log)
	}
}

func TestO37TwoPassesDecidingFromTheSameObservationChooseTheSameTarget(t *testing.T) {
	s := firstExportSim(t)
	s.obs.Guests = append(s.obs.Guests, s.newGuest(2, "c"))
	s.roles[pod2] = nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby}
	stale := &frozen{inner: s}
	a, b := s.deps(), s.deps()
	a.Observer, b.Observer = stale, stale

	_, _ = New(a).Pass(ctxBackground)
	_, _ = New(b).Pass(ctxBackground)

	for _, p := range s.promotes {
		if p.pod != pod0 {
			t.Errorf("promoted %s, want the same pod both passes chose", p.pod)
		}
	}
	if len(s.writes) != 1 {
		t.Errorf("writes = %+v, want one", s.writes)
	}
}

func TestO44TheSequenceIsRecordWithdrawPromoteServeThenReassemble(t *testing.T) {
	s := lostActiveSim(t, 3)
	s.tick()                          // held
	s.clock.advance(21 * time.Second) // the window elapsed
	runUntilQuiet(t, s, 6)

	want := []string{"record:mds-1@2", "withdraw", "promote:mds-1@2", "serve:10.0.0.2"}
	got := []string{}
	for _, l := range s.log {
		if !strings.HasPrefix(l, "rebind:") && l != "conflict" {
			got = append(got, l)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sequence = %v, want %v", got, want)
	}
	serveAt := slices.Index(s.log, "serve:10.0.0.2")
	for i, l := range s.log {
		if strings.HasPrefix(l, "rebind:") && i < serveAt {
			t.Errorf("%s ran before the EndpointSlice pointed at the target", l)
		}
	}
	if s.rebinds() != 3 {
		t.Errorf("rebinds = %d, want one per export", s.rebinds())
	}
	if !s.hasEvent("exports", "Warning", "MDSPromoted") || !s.hasEvent("driver", "Warning", "MDSPromoted") {
		t.Errorf("events = %v, want a Warning MDSPromoted on the driver and the exports for a loss", s.events)
	}
	if e, _ := s.lastEvent("MDSPromoted"); !strings.Contains(e.message, "Lost") {
		t.Errorf("MDSPromoted message = %q, want the cause", e.message)
	}
}

func TestO11ARestartAfterTheRecordPromotesAtTheRecordedEpoch(t *testing.T) {
	s := lostActiveSim(t, 2)
	s.tick()
	s.clock.advance(21 * time.Second)
	s.fault("withdraw", 1)
	if _, err := s.pass(); err == nil {
		t.Fatal("want the injected crash")
	}
	if len(s.writes) != 1 || len(s.promotes) != 0 {
		t.Fatalf("writes=%d promotes=%d, want the record written and nothing promoted", len(s.writes), len(s.promotes))
	}

	s.restart()
	runUntilQuiet(t, s, 6)

	if len(s.writes) != 1 {
		t.Errorf("record writes = %+v, want no second epoch", s.writes)
	}
	if len(s.promotes) != 1 || s.promotes[0].req.Epoch != 2 {
		t.Errorf("promotes = %+v, want one at the recorded epoch 2", s.promotes)
	}
}

func TestO12RestartsAtEveryStepFinishTheSameTakeover(t *testing.T) {
	for _, step := range []struct {
		name string
		nth  int
	}{
		{"withdraw", 1}, {"promote", 1}, {"serve", 1}, {stepRebind, 1}, {stepRebind, 2}, {stepRebind, 3},
	} {
		t.Run(step.name+"-"+string(rune('0'+step.nth)), func(t *testing.T) {
			s := lostActiveSim(t, 3)
			s.tick()
			s.clock.advance(21 * time.Second)
			s.fault(step.name, step.nth)
			s.tick() // crashes at the step
			s.restart()
			runUntilQuiet(t, s, 10)

			if len(s.writes) != 1 || s.obs.Record.Epoch != 2 || s.obs.Record.Active != pod1 {
				t.Fatalf("record = %+v writes=%+v, want one write: epoch 2 on mds-1", s.obs.Record, s.writes)
			}
			if s.roles[pod1].Role != nfsexportrpc.RoleActive {
				t.Errorf("mds-1 role = %v, want Active", s.roles[pod1].Role)
			}
			for _, e := range s.exports {
				if e.Pod != pod1 || e.AssembledBy != "u1/c1" {
					t.Errorf("export %s = %+v, want bound to and assembled by mds-1", e.Key.Name, e)
				}
			}
		})
	}
}

func TestOnlyExportsNotAssembledByTheNewInstanceAreReassembledAfterARestart(t *testing.T) {
	s := lostActiveSim(t, 4)
	s.tick()
	s.clock.advance(21 * time.Second)
	s.fault(stepRebind, 3)
	s.tick()
	s.assemble() // the resync finished the two exports rebound so far
	before := s.rebinds()
	s.restart()
	runUntilQuiet(t, s, 10)

	if got := s.rebinds() - before; got != 2 {
		t.Errorf("rebinds after the restart = %d, want only the 2 exports left", got)
	}
}

func TestO14ACollisionRaisesTheEpochAndPromotesAgain(t *testing.T) {
	s := firstExportSim(t)
	calls := 0
	s.promoteFn = func(s *sim, pod string, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
		calls++
		if calls == 1 {
			return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteCollision}, nil
		}
		return s.activate(pod, req), nil
	}

	runUntilQuiet(t, s, 8)

	if len(s.writes) != 2 || s.writes[0].Epoch != 1 || s.writes[1].Epoch != 2 || s.writes[1].Active != pod0 {
		t.Fatalf("writes = %+v, want epoch 1 then 2 on the same pod", s.writes)
	}
	if len(s.promotes) != 2 {
		t.Fatalf("promotes = %d, want a retry", len(s.promotes))
	}
	want, _ := nfsexport.MDSReservationKey(simStatefulSetUID, 2)
	if s.promotes[1].req.Epoch != 2 || s.promotes[1].req.Key != want {
		t.Errorf("retry = %+v, want epoch 2 and its key", s.promotes[1].req)
	}
}

func TestO15AtMostEightExportsReassembleAtOnce(t *testing.T) {
	s := lostActiveSim(t, 20)
	s.tick()
	s.clock.advance(21 * time.Second)

	runUntilQuiet(t, s, 12)

	if s.maxFlight > 8 {
		t.Errorf("in flight at once = %d, want at most 8", s.maxFlight)
	}
	if s.maxFlight < 8 {
		t.Errorf("in flight at most = %d, want a batch of 8 to be used", s.maxFlight)
	}
	for _, e := range s.exports {
		if e.Pod != pod1 {
			t.Errorf("export %s is on %s", e.Key.Name, e.Pod)
		}
	}
}

func TestO17ATakeoverWithNoExportsFencesTheStateVolumeAlone(t *testing.T) {
	s := lostActiveSim(t, 0)
	s.tick()
	s.clock.advance(21 * time.Second)

	runUntilQuiet(t, s, 4)

	if len(s.promotes) != 1 || len(s.promotes[0].req.Exports) != 0 {
		t.Fatalf("promotes = %+v, want one with no exports", s.promotes)
	}
	if s.roles[pod1].Role != nfsexportrpc.RoleActive {
		t.Error("the guest did not become active")
	}
}

func TestO18AndO19ThePassHoldsUntilTheWindowElapses(t *testing.T) {
	s := lostActiveSim(t, 1)

	s.tick()
	s.clock.advance(10 * time.Second)
	s.tick()
	if len(s.writes) != 0 || !s.hasEvent("driver", "Normal", "MDSFailoverHeld") {
		t.Fatalf("inside the window: writes=%v events=%v, want held", s.writes, s.reasons())
	}

	s.clock.advance(10 * time.Second) // exactly the window
	s.tick()
	if len(s.writes) != 0 {
		t.Fatal("took over at exactly the window")
	}
	s.clock.advance(time.Millisecond)
	s.tick()
	if len(s.writes) != 1 {
		t.Fatalf("writes = %+v, want the takeover one tick past the window", s.writes)
	}
}

func TestO20APassWithAStaleReadyConditionAndALiveSessionDoesNothing(t *testing.T) {
	s := newSim(t)
	s.serving(pod0, 1)
	s.guest(pod0).Ready = false
	s.guest(pod0).NodeReady = false
	for range 6 {
		s.tick()
		s.clock.advance(30 * time.Second)
	}
	if len(s.writes) != 0 || s.hasEvent("driver", "Normal", "MDSFailoverHeld") {
		t.Errorf("writes=%v events=%v, want nothing", s.writes, s.reasons())
	}
}

func TestO21ABrokenLinkAloneDoesNothing(t *testing.T) {
	s := newSim(t)
	s.serving(pod0, 1)
	s.guest(pod0).HasSession = false
	for range 6 {
		s.tick()
		s.clock.advance(30 * time.Second)
	}
	if len(s.writes) != 0 {
		t.Errorf("writes = %v, want none", s.writes)
	}
}

func TestO22ALeaderChangeRestartsTheWindow(t *testing.T) {
	s := lostActiveSim(t, 1)
	s.tick()
	s.clock.advance(60 * time.Second)

	s.failover.LeadershipAcquired()
	s.tick()
	if len(s.writes) != 0 {
		t.Fatal("took over on the first observation after leadership")
	}
	s.clock.advance(19 * time.Second)
	s.tick()
	if len(s.writes) != 0 {
		t.Fatal("took over inside the window that began at leadership")
	}
	s.clock.advance(2 * time.Second)
	s.tick()
	if len(s.writes) != 1 {
		t.Errorf("writes = %v, want the takeover once the new window elapsed", s.writes)
	}
}

func TestO23APodWithADeletionTimestampStartsTheTakeoverAtOnce(t *testing.T) {
	s := newSim(t)
	s.serving(pod0, 1)
	s.bind(2)
	s.guest(pod0).Deleting = true

	runUntilQuiet(t, s, 6)

	if len(s.writes) != 1 || s.writes[0].Active != pod1 {
		t.Fatalf("writes = %+v, want an immediate takeover to mds-1", s.writes)
	}
	if s.hasEvent("driver", "Normal", "MDSFailoverHeld") {
		t.Error("a planned loss was held")
	}
	if !s.hasEvent("driver", "Normal", "MDSPromoted") {
		t.Errorf("events = %v, want a Normal MDSPromoted for a planned takeover", s.reasons())
	}
}

func TestO24ALossWithNoStandbyWaitsAndThePromotesTheFirstGuestBack(t *testing.T) {
	s := lostActiveSim(t, 1)
	s.guest(pod1).HasSession = false

	for range 3 {
		s.tick()
		s.clock.advance(30 * time.Second)
	}
	if len(s.writes) != 0 || !s.hasEvent("driver", "Warning", "MDSStandbyMissing") {
		t.Fatalf("writes=%v events=%v, want a wait with MDSStandbyMissing", s.writes, s.reasons())
	}

	s.guest(pod1).HasSession = true
	s.tick()
	s.clock.advance(21 * time.Second)
	runUntilQuiet(t, s, 8)
	if len(s.writes) != 1 || s.writes[0].Active != pod1 {
		t.Errorf("writes = %+v, want the first guest back promoted", s.writes)
	}
}

func TestARestartedActiveIsPromotedAgainAtOnce(t *testing.T) {
	// Replicas 1: the only guest rebooted in place and reports Standby with a
	// boot that is not the recorded one.
	s := newSim(t)
	s.obs.Guests = s.obs.Guests[:1]
	s.obs.Replicas = 1
	s.serving(pod0, 1)
	s.bind(1)
	g := s.guest(pod0)
	g.Instance = "u0/c0-restarted"
	s.roles[pod0] = nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby}

	runUntilQuiet(t, s, 6)

	if len(s.writes) != 1 || s.writes[0].Epoch != 2 || s.writes[0].Active != pod0 {
		t.Errorf("writes = %+v, want epoch 2 on the restarted pod", s.writes)
	}
	if s.obs.Record.Instance != "u0/c0-restarted" {
		t.Errorf("recorded instance = %q, want the new boot", s.obs.Record.Instance)
	}
}

func TestO25AReturningOldActiveIsDemoted(t *testing.T) {
	s := newSim(t)
	s.serving(pod1, 2)
	s.roles[pod0] = nfsexportrpc.Description{Role: nfsexportrpc.RoleActive, Epoch: 1}

	s.tick()

	if !slices.Equal(s.deleted, []string{pod0}) {
		t.Errorf("deleted = %v, want the returning old active mds-0", s.deleted)
	}
	if !s.hasEvent("driver", "Normal", "MDSDemoted") {
		t.Errorf("events = %v, want MDSDemoted", s.reasons())
	}
}

func TestTheRecordedActiveIsNeverDemoted(t *testing.T) {
	s := newSim(t)
	s.serving(pod0, 1)
	s.tick()
	if len(s.deleted) != 0 {
		t.Errorf("deleted = %v, want none", s.deleted)
	}
}

func TestO26TheStuckOldPodDoesNotBlockTheTakeover(t *testing.T) {
	s := lostActiveSim(t, 1)
	s.guest(pod0).Deleting = true // Terminating on the lost node

	runUntilQuiet(t, s, 6)

	if s.obs.Record.Active != pod1 {
		t.Fatalf("active = %q, want mds-1", s.obs.Record.Active)
	}
	// With the old pod stuck, the loss keeps being reported as a missing standby
	// by the pass that has no candidate, and the active keeps serving.
	s.tick()
	if s.obs.Record.Active != pod1 || len(s.writes) != 1 {
		t.Errorf("record = %+v writes = %v, want the takeover left as it was", s.obs.Record, s.writes)
	}
}

func TestO45AFenceFailureRetriesAtTheSameEpochWithBackoff(t *testing.T) {
	s := firstExportSim(t)
	failing := true
	s.promoteFn = func(s *sim, pod string, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
		if failing {
			return nfsexportrpc.PromoteResult{
				Outcome: nfsexportrpc.PromoteFenceFailed, Device: "vol-3", Step: "preempt",
			}, nil
		}
		return s.activate(pod, req), nil
	}

	res, err := s.pass()
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if res.RequeueAfter < time.Second {
		t.Errorf("requeue = %v, want a backoff", res.RequeueAfter)
	}
	if s.logged("serve:10.0.0.1") || s.rebinds() != 0 {
		t.Errorf("log = %v, want nothing served after a failed fence", s.log)
	}
	for _, scope := range []string{"driver", "exports"} {
		if !s.hasEvent(scope, "Warning", "MDSFenceFailed") {
			t.Errorf("no MDSFenceFailed on %s: %v", scope, s.events)
		}
	}
	if e, _ := s.lastEvent("MDSFenceFailed"); !strings.Contains(e.message, "vol-3") || !strings.Contains(e.message, "preempt") {
		t.Errorf("message = %q, want the device and the step", e.message)
	}

	s.clock.advance(time.Second)
	s.tick()
	if len(s.promotes) != 1 {
		t.Errorf("promotes = %d one second into the backoff, want no retry yet", len(s.promotes))
	}

	failing = false
	s.clock.advance(10 * time.Second)
	runUntilQuiet(t, s, 4)
	if len(s.promotes) != 2 || s.promotes[1].req.Epoch != 1 || len(s.writes) != 1 {
		t.Errorf("promotes=%d writes=%v, want the retry at the same epoch 1", len(s.promotes), s.writes)
	}
	if got := metricValue(t, s.reg, "simplyblock_pnfs_mds_failovers_total",
		map[string]string{"cause": "First", "result": "FenceFailed"}); got != 1 {
		t.Errorf("FenceFailed count = %v, want 1 per takeover", got)
	}
}

func TestASupersededPromotionServesTheNewerActiveInstead(t *testing.T) {
	s := firstExportSim(t)
	s.promoteFn = func(s *sim, pod string, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
		// Another writer recorded mds-1 at epoch 5 meanwhile.
		s.serving(pod1, 5)
		return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteSuperseded}, nil
	}

	s.tick()
	if !s.hasEvent("driver", "Warning", "MDSFenceFailed") {
		t.Errorf("events = %v, want MDSFenceFailed for a refused promotion", s.reasons())
	}
	s.promoteFn = nil
	runUntilQuiet(t, s, 4)

	if len(s.writes) != 1 {
		t.Errorf("writes = %v, want none after the superseding one", s.writes)
	}
	if !s.logged("serve:10.0.0.2") {
		t.Errorf("log = %v, want the newer active served", s.log)
	}
}

func TestAStaleEpochRereadsWithoutAnEventOrAWrite(t *testing.T) {
	s := firstExportSim(t)
	s.promoteFn = func(*sim, string, nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
		return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteStaleEpoch}, nil
	}
	res, err := s.pass()
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("pass = %+v, %v, want a quick requeue", res, err)
	}
	if s.hasEvent("driver", "Warning", "MDSFenceFailed") {
		t.Error("a stale epoch is a re-read, not a fence failure")
	}
	if len(s.writes) != 1 {
		t.Errorf("writes = %v, want only the first record", s.writes)
	}
}

func TestO39AFailedReadCountsTowardNoLossAndRestartsTheWindow(t *testing.T) {
	s := lostActiveSim(t, 1)
	s.tick()
	s.clock.advance(15 * time.Second)

	s.observeErr = errTest
	if _, err := s.pass(); err == nil {
		t.Fatal("want the read error back")
	}
	s.clock.advance(30 * time.Second)
	s.observeErr = nil

	s.tick()
	if len(s.writes) != 0 {
		t.Fatal("took over although a read failed in between: the window must not advance")
	}
	s.clock.advance(21 * time.Second)
	s.tick()
	if len(s.writes) != 1 {
		t.Errorf("writes = %v, want the takeover after a full uninterrupted window", s.writes)
	}
}

func TestO30TheEventsGoToTheObjectsTheDesignNames(t *testing.T) {
	s := lostActiveSim(t, 1)
	s.tick()
	s.clock.advance(21 * time.Second)
	runUntilQuiet(t, s, 6)

	for _, reason := range []string{"MDSFailoverHeld", "MDSFenced"} {
		if !s.hasEvent("driver", "Normal", reason) {
			t.Errorf("no Normal %s on the driver: %v", reason, s.reasons())
		}
		if s.hasEvent("exports", "Normal", reason) {
			t.Errorf("%s went to the exports, which the design does not list", reason)
		}
	}
	if e, ok := s.lastEvent("MDSFenced"); !ok || !strings.Contains(e.message, "2 ") {
		t.Errorf("MDSFenced = %+v, want the count of fenced devices (state volume and one export)", e)
	}
}

func TestO31TheMetricsCountAndTimeATakeover(t *testing.T) {
	s := lostActiveSim(t, 2)
	s.tick()
	s.clock.advance(21 * time.Second)
	runUntilQuiet(t, s, 6)

	if got := metricValue(t, s.reg, "simplyblock_pnfs_mds_failovers_total",
		map[string]string{labelCluster: simCluster, "cause": "Lost", "result": "Succeeded"}); got != 1 {
		t.Errorf("failovers_total{Lost,Succeeded} = %v, want 1", got)
	}
	if got := metricValue(t, s.reg, "simplyblock_pnfs_mds_epoch_state", map[string]string{labelCluster: simCluster}); got != 2 {
		t.Errorf("epoch = %v, want 2", got)
	}
	for _, phase := range []string{PhaseDetect, PhaseFence, PhaseReassemble, PhaseServe} {
		if n := histogramCount(t, s.reg, "simplyblock_pnfs_mds_takeover_seconds",
			map[string]string{labelCluster: simCluster, "phase": phase}); n != 1 {
			t.Errorf("takeover_seconds{%s} observed %d times, want once per takeover", phase, n)
		}
	}
	if got := metricValue(t, s.reg, "simplyblock_pnfs_mds_role_state",
		map[string]string{labelCluster: simCluster, "pod": pod1, "role": "Active"}); got != 1 {
		t.Errorf("role{mds-1,Active} = %v, want 1", got)
	}
	if got := metricValue(t, s.reg, "simplyblock_pnfs_mds_role_state",
		map[string]string{labelCluster: simCluster, "pod": pod1, "role": "Standby"}); got != 0 {
		t.Errorf("role{mds-1,Standby} = %v, want 0", got)
	}
}

func TestAPromoteTransportErrorIsReturnedForBackoff(t *testing.T) {
	s := firstExportSim(t)
	s.fault("promote", 1)
	if _, err := s.pass(); err == nil {
		t.Fatal("want the transport error back")
	}
	runUntilQuiet(t, s, 4)
	if len(s.writes) != 1 || s.roles[pod0].Role != nfsexportrpc.RoleActive {
		t.Errorf("writes=%v roles=%v, want the retry to promote at the same epoch", s.writes, s.roles)
	}
}

func TestAStateVolumeWhoseHostsAreNotAllowedHoldsThePromotion(t *testing.T) {
	s := firstExportSim(t)
	allowed := false
	deps := s.deps()
	deps.HostsAllowed = func(atlaslvol.VolumeHandle) bool { return allowed }
	s.failover = New(deps)

	res, err := s.pass()
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("pass = %+v, %v, want a wait", res, err)
	}
	if len(s.promotes) != 0 || s.logged("withdraw") {
		t.Errorf("log = %v, want nothing withdrawn or promoted before the hosts are allowed", s.log)
	}
	if len(s.writes) != 1 {
		t.Errorf("writes = %v, want the record written ahead, as the takeover is decided", s.writes)
	}

	allowed = true
	runUntilQuiet(t, s, 4)
	if len(s.promotes) != 1 || s.promotes[0].req.Epoch != 1 {
		t.Errorf("promotes = %+v, want the promotion at the recorded epoch once allowed", s.promotes)
	}
}

func countOf(reasons []string, reason string) int {
	n := 0
	for _, r := range reasons {
		if r == reason {
			n++
		}
	}
	return n
}

// A takeover is announced once, when the target serves every export. The passes
// that follow, which happen every few seconds for as long as the cluster runs,
// owe nothing and say nothing.
func TestATakeoverIsAnnouncedOnceAndNotOnEveryPassAfterIt(t *testing.T) {
	s := lostActiveSim(t, 3)
	s.tick()
	s.clock.advance(21 * time.Second)
	runUntilQuiet(t, s, 6)
	announced := countOf(s.reasons(), "MDSPromoted")
	if announced != 2 {
		t.Fatalf("MDSPromoted events = %d, want one on the driver and one on the exports", announced)
	}

	for i := 0; i < 20; i++ {
		s.tick()
		s.clock.advance(5 * time.Second)
	}

	if got := countOf(s.reasons(), "MDSPromoted"); got != announced {
		t.Errorf("MDSPromoted events = %d after 20 settled passes, want %d", got, announced)
	}
	if got := metricValue(t, s.reg, "simplyblock_pnfs_mds_failovers_total",
		map[string]string{labelCluster: simCluster, "cause": "Lost", "result": "Succeeded"}); got != 1 {
		t.Errorf("failovers_total{Lost,Succeeded} = %v after settled passes, want 1", got)
	}
}

// An operator that restarts on a settled cluster has nothing to resume, so it
// announces no takeover.
func TestARestartOnASettledClusterAnnouncesNothing(t *testing.T) {
	s := lostActiveSim(t, 3)
	s.tick()
	s.clock.advance(21 * time.Second)
	runUntilQuiet(t, s, 6)
	before := countOf(s.reasons(), "MDSPromoted")

	s.restart()
	for i := 0; i < 10; i++ {
		s.tick()
		s.clock.advance(5 * time.Second)
	}

	if got := countOf(s.reasons(), "MDSPromoted"); got != before {
		t.Errorf("MDSPromoted events = %d after a restart, want %d", got, before)
	}
}

// A frozen guest keeps its runner's link session, so the session condition never
// holds. The guest not answering, with its pod not Ready, is the loss.
func TestAHungActiveThatKeepsItsSessionIsReplacedAfterTheWindow(t *testing.T) {
	s := newSim(t)
	s.serving(pod0, 1)
	s.bind(2)
	s.hang(pod0)

	s.tick()
	if len(s.writes) != 0 {
		t.Fatalf("record written inside the window: %+v", s.writes)
	}
	if !s.hasEvent("driver", "Normal", "MDSFailoverHeld") {
		t.Errorf("events = %v, want MDSFailoverHeld while the loss is observed", s.reasons())
	}
	s.clock.advance(21 * time.Second)
	runUntilQuiet(t, s, 6)

	if len(s.writes) != 1 || s.writes[0].Active != pod1 || s.writes[0].Epoch != 2 {
		t.Fatalf("record writes = %+v, want one naming mds-1 at epoch 2", s.writes)
	}
	if len(s.promotes) != 1 || s.promotes[0].pod != pod1 {
		t.Errorf("promotes = %+v, want mds-1 promoted", s.promotes)
	}
}

func TestAnActiveThatAnswersButIsNotReadyIsNotReplaced(t *testing.T) {
	s := newSim(t)
	s.serving(pod0, 1)
	s.bind(2)
	s.guest(pod0).Ready = false

	for i := 0; i < 8; i++ {
		s.tick()
		s.clock.advance(15 * time.Second)
	}
	if len(s.writes) != 0 || len(s.promotes) != 0 {
		t.Errorf("replaced an active that answers: writes=%+v promotes=%+v", s.writes, s.promotes)
	}
}

func TestAReadyActiveThatMissesAnAnswerIsNotReplaced(t *testing.T) {
	s := newSim(t)
	s.serving(pod0, 1)
	s.bind(2)
	s.silent = map[string]bool{pod0: true}

	for i := 0; i < 8; i++ {
		s.tick()
		s.clock.advance(15 * time.Second)
	}
	if len(s.writes) != 0 || len(s.promotes) != 0 {
		t.Errorf("replaced a Ready active over a missed answer: writes=%+v promotes=%+v", s.writes, s.promotes)
	}
}
