package smtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/statemachine"
)

// phase is a stand-in for the state type a caller would use — typically a
// resource's own phase enum.
type phase string

const (
	pending    phase = "Pending"
	validating phase = "Validating"
	running    phase = "Running"
	completed  phase = "Completed"
	failed     phase = "Failed"
	orphan     phase = "Orphan"
)

// fakeTB records what a helper reported instead of failing the real test. Only the
// reporting methods this package uses are overridden; anything else falls through to
// the embedded testing.TB and would fail loudly if a helper reached for it.
type fakeTB struct {
	testing.TB
	errors []string
	fatals []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatals = append(f.fatals, fmt.Sprintf(format, args...))
}

func (f *fakeTB) assertNoReports(t *testing.T) {
	t.Helper()
	if len(f.errors) > 0 {
		t.Errorf("helper reported %d unexpected error(s): %v", len(f.errors), f.errors)
	}
	if len(f.fatals) > 0 {
		t.Errorf("helper reported %d unexpected fatal(s): %v", len(f.fatals), f.fatals)
	}
}

// assertReported reports unless exactly one error was recorded, mentioning want.
func (f *fakeTB) assertReported(t *testing.T, want string) {
	t.Helper()
	if len(f.errors) != 1 {
		t.Fatalf("recorded %d error(s), want exactly 1: %v", len(f.errors), f.errors)
	}
	if !strings.Contains(f.errors[0], want) {
		t.Errorf("error %q does not mention %q", f.errors[0], want)
	}
}

// graph is the migration-shaped graph the tests assert against: a linear happy path,
// a failure edge from every non-terminal phase, and (optionally) a state no edge
// points at.
func graph(probe *Probe[phase], withOrphan bool) statemachine.Config[phase] {
	states := map[phase]statemachine.StateDef[phase]{
		pending:    {To: []phase{pending, validating, failed}, OnEnter: probe.Hook(time.Minute)},
		validating: {To: []phase{running, failed}, OnEnter: probe.Hook(10 * time.Minute)},
		running:    {To: []phase{completed, failed}, OnEnter: probe.Hook(0)},
		completed:  {OnEnter: probe.Hook(0)},
		failed:     {OnEnter: probe.Hook(0)},
	}
	if withOrphan {
		states[orphan] = statemachine.StateDef[phase]{To: []phase{failed}, OnEnter: probe.Hook(0)}
	}
	return statemachine.Config[phase]{Initial: pending, States: states}
}

func newMachine(t *testing.T, probe *Probe[phase], withOrphan bool) *statemachine.Machine[phase] {
	t.Helper()
	sm := statemachine.Must(t.Context(), graph(probe, withOrphan))
	t.Cleanup(sm.Close)
	return sm
}

// --- Probe -------------------------------------------------------------------

func TestProbeRecordsPath(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	for _, to := range []phase{validating, running, completed} {
		if err := sm.TransitionTo(t.Context(), to); err != nil {
			t.Fatalf("TransitionTo(%v): %v", to, err)
		}
	}

	probe.AssertPath(t, validating, running, completed)
	probe.AssertEntered(t, running, 1)
	probe.AssertEntered(t, pending, 0) // the initial state is never entered

	entries := probe.Entries()
	if len(entries) != 3 {
		t.Fatalf("recorded %d entries, want 3", len(entries))
	}
	if entries[0].From != pending || entries[0].To != validating {
		t.Errorf("first entry = %v, want pending -> validating", entries[0])
	}
	if got := entries[0].String(); got != "Pending -> Validating" {
		t.Errorf("Entry.String() = %q, want %q", got, "Pending -> Validating")
	}
}

// A self-transition re-runs the hook, so the probe must count it again rather than
// collapsing it into the previous entry.
func TestProbeCountsSelfTransitions(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	for range 3 {
		if err := sm.TransitionTo(t.Context(), pending); err != nil {
			t.Fatalf("self-transition: %v", err)
		}
	}
	probe.AssertEntered(t, pending, 3)
	probe.AssertPath(t, pending, pending, pending)
}

// A hook that refuses the transition still ran, so it is still recorded — while the
// machine stays where it was.
func TestProbeFailRecordsAndRefuses(t *testing.T) {
	var probe Probe[phase]
	sentinel := errors.New("CreateMigration: cluster is rebalancing")

	sm := statemachine.Must(t.Context(), statemachine.Config[phase]{
		Initial: pending,
		States: map[phase]statemachine.StateDef[phase]{
			pending:    {To: []phase{validating}},
			validating: {OnEnter: probe.Fail(sentinel)},
		},
	})
	t.Cleanup(sm.Close)

	err := sm.TransitionTo(t.Context(), validating)
	if !errors.Is(err, sentinel) {
		t.Fatalf("TransitionTo error = %v, want it to wrap the hook's own error", err)
	}
	if got := sm.CurrentState(); got != pending {
		t.Errorf("machine is in %v, want it left in %v", got, pending)
	}
	probe.AssertPath(t, validating)
}

func TestProbeHookFuncDelegates(t *testing.T) {
	var probe Probe[phase]
	var gotFrom, gotTo phase

	sm := statemachine.Must(t.Context(), statemachine.Config[phase]{
		Initial: pending,
		States: map[phase]statemachine.StateDef[phase]{
			pending: {To: []phase{validating}},
			validating: {OnEnter: probe.HookFunc(func(_ context.Context, from, to phase) (time.Duration, error) {
				gotFrom, gotTo = from, to
				return 42 * time.Second, nil
			})},
		},
	})
	t.Cleanup(sm.Close)

	if err := sm.TransitionTo(t.Context(), validating); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	if gotFrom != pending || gotTo != validating {
		t.Errorf("delegate saw %v -> %v, want %v -> %v", gotFrom, gotTo, pending, validating)
	}
	Check(t, sm).DeadlineIn(42*time.Second, time.Second)
	probe.AssertPath(t, validating)
}

func TestProbeReset(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	if err := sm.TransitionTo(t.Context(), validating); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	probe.Reset()
	probe.AssertPath(t) // no arguments: nothing recorded since the reset

	if err := sm.TransitionTo(t.Context(), running); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	probe.AssertPath(t, running)
}

func TestProbeAssertPathReportsMismatch(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)
	if err := sm.TransitionTo(t.Context(), validating); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}

	tb := &fakeTB{TB: t}
	probe.AssertPath(tb, running)
	tb.assertReported(t, "want [Running]")
}

// --- Checker -----------------------------------------------------------------

func TestCheckerHappyPath(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	tb := &fakeTB{TB: t}
	c := Check(tb, sm)
	c.In(pending).NotTerminal().NoDeadline().NotTimedOut().
		Allows(pending, validating, failed).Reachable()
	c.Enters(t.Context(), validating).
		In(validating).DeadlineIn(10*time.Minute, time.Second).NotTimedOut()
	c.Enters(t.Context(), running).NoDeadline().Allows(completed, failed)
	c.Enters(t.Context(), completed).Terminal().Allows()
	tb.assertNoReports(t)
}

func TestCheckerInReportsWrongState(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	tb := &fakeTB{TB: t}
	Check(tb, sm).In(running)
	tb.assertReported(t, "is in Pending, want Running")
}

func TestCheckerTerminalReportsBothWays(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	tb := &fakeTB{TB: t}
	Check(tb, sm).Terminal()
	tb.assertReported(t, "is not terminal")

	if err := sm.TransitionTo(t.Context(), failed); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	tb2 := &fakeTB{TB: t}
	Check(tb2, sm).NotTerminal()
	tb2.assertReported(t, "is terminal")
}

// Allows compares the edge set regardless of declaration order, and reports the
// whole set when it disagrees rather than the first difference.
func TestCheckerAllows(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	tb := &fakeTB{TB: t}
	Check(tb, sm).Allows(failed, validating, pending)
	tb.assertNoReports(t)

	for _, want := range [][]phase{
		{validating},                          // too few
		{pending, validating, failed, orphan}, // too many
		{pending, validating, running},        // same size, wrong member
	} {
		tb := &fakeTB{TB: t}
		Check(tb, sm).Allows(want...)
		if len(tb.errors) != 1 {
			t.Errorf("Allows(%v) recorded %d error(s), want 1", want, len(tb.errors))
		}
	}
}

// A state no edge points at is a typo in a To list, and the point of Reachable.
func TestCheckerReachableFindsOrphan(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, true)

	tb := &fakeTB{TB: t}
	Check(tb, sm).Reachable()
	tb.assertReported(t, "Orphan")
}

func TestCheckerDeadlineAssertions(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	// A state without a deadline: NoDeadline passes, DeadlineIn reports.
	tb := &fakeTB{TB: t}
	Check(tb, sm).NoDeadline()
	tb.assertNoReports(t)

	tb = &fakeTB{TB: t}
	Check(tb, sm).DeadlineIn(time.Minute, time.Second)
	tb.assertReported(t, "has no deadline")

	// A state with one: the reverse.
	if err := sm.TransitionTo(t.Context(), validating); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	tb = &fakeTB{TB: t}
	Check(tb, sm).NoDeadline()
	tb.assertReported(t, "want no deadline")

	tb = &fakeTB{TB: t}
	Check(tb, sm).DeadlineIn(time.Minute, time.Second)
	tb.assertReported(t, "want 1m0s ± 1s")
}

func TestCheckerEntersFailsFatallyOnIllegalEdge(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	tb := &fakeTB{TB: t}
	Check(tb, sm).Enters(t.Context(), completed)
	if len(tb.fatals) != 1 || !strings.Contains(tb.fatals[0], "Pending -> Completed") {
		t.Errorf("fatals = %v, want one naming the refused edge", tb.fatals)
	}
	probe.AssertPath(t) // the hook never ran
}

func TestCheckerRefuses(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	// An edge the graph does not declare.
	tb := &fakeTB{TB: t}
	Check(tb, sm).Refuses(t.Context(), completed).In(pending)
	tb.assertNoReports(t)

	// An edge it does: Refuses must not let it pass.
	tb = &fakeTB{TB: t}
	Check(tb, sm).Refuses(t.Context(), validating)
	if len(tb.fatals) != 1 || !strings.Contains(tb.fatals[0], "want it refused") {
		t.Errorf("fatals = %v, want one reporting the transition succeeded", tb.fatals)
	}
}

// A hook that fails is not the graph refusing an edge, and Refuses says so — the
// distinction is the whole reason IllegalTransitionError is its own type.
func TestCheckerRefusesRejectsHookFailure(t *testing.T) {
	var probe Probe[phase]
	sm := statemachine.Must(t.Context(), statemachine.Config[phase]{
		Initial: pending,
		States: map[phase]statemachine.StateDef[phase]{
			pending:    {To: []phase{validating}},
			validating: {OnEnter: probe.Fail(errors.New("boom"))},
		},
	})
	t.Cleanup(sm.Close)

	tb := &fakeTB{TB: t}
	Check(tb, sm).Refuses(t.Context(), validating)
	tb.assertReported(t, "want an IllegalTransitionError")
}

func TestCheckerMachineIsTheMachineUnderTest(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)
	if got := Check(t, sm).Machine(); got != sm {
		t.Errorf("Machine() returned a different machine")
	}
}

// --- Expire ------------------------------------------------------------------

func TestExpire(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	if err := sm.TransitionTo(t.Context(), validating); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	c := Check(t, sm)
	c.NotTimedOut()

	Expire(t, sm)
	c.In(validating).TimedOut() // expiring is not a transition

	// The machine is still usable afterwards: a timeout is a decision to make, not
	// a broken machine.
	c.Enters(t.Context(), failed).In(failed)
}

// A state with no deadline gets one, since a test calling Expire wants a timeout
// either way.
func TestExpireArmsAnUnboundedState(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	Check(t, sm).NoDeadline()
	Expire(t, sm)
	Check(t, sm).TimedOut()
	probe.AssertPath(t) // Expire runs no hook
}

func TestTimedOutReportsWhenNotExpired(t *testing.T) {
	var probe Probe[phase]
	sm := newMachine(t, &probe, false)

	tb := &fakeTB{TB: t}
	Check(tb, sm).TimedOut()
	tb.assertReported(t, "has no deadline")

	if err := sm.TransitionTo(t.Context(), validating); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	tb = &fakeTB{TB: t}
	Check(tb, sm).TimedOut()
	tb.assertReported(t, "expires in")

	Expire(t, sm)
	tb = &fakeTB{TB: t}
	Check(tb, sm).NotTimedOut()
	tb.assertReported(t, "has timed out")
}
