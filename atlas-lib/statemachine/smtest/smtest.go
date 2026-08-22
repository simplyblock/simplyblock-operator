// Package smtest provides test helpers for state machines built with
// [github.com/simplyblock/atlas/statemachine].
//
// It exists as its own package so that [statemachine] itself does not import
// [testing], following the convention of httptest, iotest and fstest. Nothing here
// reaches into the machine's internals; it is written against the same public API a
// caller has, so a helper can always be replaced by hand-written assertions.
//
// Two things are on offer. A [Probe] stands in for the entry hooks of a graph and
// records what the machine did, so a test can assert on the path taken rather than
// on the state it ended in. A [Checker] wraps a machine and a [testing.TB] and
// turns the questions a test asks about a machine — which state, which deadline,
// which edges — into assertions that report themselves.
//
// # Example
//
// Asserting that a graph refuses the edge it is supposed to refuse, and that
// entering a state arms the bound its hook promised:
//
//	func TestMigrationGraph(t *testing.T) {
//		var probe smtest.Probe[phase]
//		sm := statemachine.Must(t.Context(), statemachine.Config[phase]{
//			Initial: pending,
//			States: map[phase]statemachine.StateDef[phase]{
//				pending:    {To: []phase{validating, failed}},
//				validating: {To: []phase{running, failed}, OnEnter: probe.Hook(10 * time.Minute)},
//				running:    {To: []phase{completed, failed}, OnEnter: probe.Hook(0)},
//				completed:  {},
//				failed:     {},
//			},
//		})
//		defer sm.Close()
//
//		c := smtest.Check(t, sm)
//		c.Reachable().NoDeadline()
//		c.Refuses(t.Context(), running) // pending cannot skip validation
//		c.Enters(t.Context(), validating).DeadlineIn(10*time.Minute, time.Second)
//		c.Enters(t.Context(), running).NoDeadline().Allows(completed, failed)
//		probe.AssertPath(t, validating, running)
//	}
//
// # Timeouts
//
// Testing what a machine does when a state runs out of time does not require
// waiting for it to: [Expire] backdates the current state's deadline, so the very
// next [statemachine.Machine.TimeoutReached] reports true.
//
//	c.Enters(t.Context(), validating)
//	smtest.Expire(t, sm)
//	c.TimedOut()
//
// # Reporting
//
// The assertion methods report with [testing.TB.Errorf] and carry on, so one run
// surfaces every disagreement rather than only the first. The methods that *act* on
// the machine — [Checker.Enters], [Checker.Refuses] and [Expire] — report with
// [testing.TB.Fatalf] instead: a transition that did not happen makes every later
// assertion about the machine meaningless.
package smtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplyblock/atlas/statemachine"
)

// Entry is one recorded entry into a state: the edge the machine took.
type Entry[S comparable] struct {
	// From is the state the machine was in when the hook ran.
	From S
	// To is the state being entered, and therefore the state whose hook recorded
	// this entry.
	To S
}

// String renders the edge as "from -> to".
func (e Entry[S]) String() string {
	return fmt.Sprintf("%v -> %v", e.From, e.To)
}

// Probe is a recording stand-in for the entry hooks of a state graph. It hands out
// [statemachine.TransitionFunc] values with [Probe.Hook] and [Probe.Fail] and
// remembers every call, so a test can assert on the path a machine took instead of
// only on where it stopped.
//
// One Probe usually serves a whole graph: because a hook records both endpoints,
// giving every state the same probe still yields an unambiguous path. Give a state
// its own Probe when the test needs to count entries into that state alone.
//
// The zero Probe is ready to use. It is safe for concurrent use, so a hook that a
// machine drives from another goroutine can share one with the test asserting on it.
type Probe[S comparable] struct {
	mu      sync.Mutex
	entries []Entry[S]
}

// Hook returns an entry hook that records the transition and reports bound as the
// new state's deadline. A bound of zero or less leaves the state without one, as
// [statemachine.TransitionFunc] specifies.
func (p *Probe[S]) Hook(bound time.Duration) statemachine.TransitionFunc[S] {
	return func(_ context.Context, from, to S) (time.Duration, error) {
		p.record(from, to)
		return bound, nil
	}
}

// Fail returns an entry hook that records the transition and then refuses it with
// err, for testing what a caller does when entering a state fails. The transition
// is still recorded: the hook ran, which is the fact a test about a failed entry
// most often needs to establish.
func (p *Probe[S]) Fail(err error) statemachine.TransitionFunc[S] {
	return func(_ context.Context, from, to S) (time.Duration, error) {
		p.record(from, to)
		return 0, err
	}
}

// HookFunc returns an entry hook that records the transition and then delegates to
// fn, for a state whose bound or outcome the test computes per call.
func (p *Probe[S]) HookFunc(fn statemachine.TransitionFunc[S]) statemachine.TransitionFunc[S] {
	return func(ctx context.Context, from, to S) (time.Duration, error) {
		p.record(from, to)
		return fn(ctx, from, to)
	}
}

func (p *Probe[S]) record(from, to S) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, Entry[S]{From: from, To: to})
}

// Entries returns the recorded entries in order. The result is a copy, so a test
// may keep it across further transitions.
func (p *Probe[S]) Entries() []Entry[S] {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.entries)
}

// Path returns the states entered, in order — the [Entry.To] of every recorded
// entry. It is what most tests want: the route the machine took, without the
// redundant predecessors.
//
// Note that the path does not begin with the initial state, which is never entered:
// a machine is born already in it and its hook does not run.
func (p *Probe[S]) Path() []S {
	entries := p.Entries()
	path := make([]S, len(entries))
	for i, e := range entries {
		path[i] = e.To
	}
	return path
}

// Count returns how many times the machine entered state, counting a
// self-transition each time it re-ran the hook.
func (p *Probe[S]) Count(state S) int {
	n := 0
	for _, e := range p.Entries() {
		if e.To == state {
			n++
		}
	}
	return n
}

// Reset forgets every recorded entry, for a test that reuses a machine across
// subtests or asserts on one phase of a longer run at a time.
func (p *Probe[S]) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = nil
}

// AssertPath reports unless the states entered are exactly want, in order. Pass no
// states to assert that the machine has not moved at all.
func (p *Probe[S]) AssertPath(tb testing.TB, want ...S) {
	tb.Helper()
	if got := p.Path(); !slices.Equal(got, want) {
		tb.Errorf("machine took path %s, want %s", render(got), render(want))
	}
}

// AssertEntered reports unless the machine entered state exactly want times.
func (p *Probe[S]) AssertEntered(tb testing.TB, state S, want int) {
	tb.Helper()
	if got := p.Count(state); got != want {
		tb.Errorf("entered %v %d time(s), want %d (path: %s)", state, got, want, render(p.Path()))
	}
}

// Checker asserts on a machine. Create one with [Check]; it holds no state of its
// own beyond the machine and the test, so a single Checker serves for the whole
// test and every method returns it for chaining:
//
//	smtest.Check(t, sm).In(validating).DeadlineIn(10*time.Minute, time.Second)
type Checker[S comparable] struct {
	tb testing.TB
	sm *statemachine.Machine[S]
}

// Check returns a [Checker] asserting on sm and reporting through tb.
func Check[S comparable](tb testing.TB, sm *statemachine.Machine[S]) *Checker[S] {
	tb.Helper()
	if sm == nil {
		tb.Fatalf("smtest.Check: nil machine")
	}
	return &Checker[S]{tb: tb, sm: sm}
}

// Machine returns the machine under test, for the assertions this package does not
// cover.
func (c *Checker[S]) Machine() *statemachine.Machine[S] {
	return c.sm
}

// In reports unless the machine is in state want.
func (c *Checker[S]) In(want S) *Checker[S] {
	c.tb.Helper()
	if got := c.sm.CurrentState(); got != want {
		c.tb.Errorf("machine is in %v, want %v", got, want)
	}
	return c
}

// Terminal reports unless the current state has no outgoing edges.
func (c *Checker[S]) Terminal() *Checker[S] {
	c.tb.Helper()
	if !c.sm.IsTerminal() {
		c.tb.Errorf("state %v is not terminal; it can still reach %s",
			c.sm.CurrentState(), render(slices.Collect(c.sm.AllowedTransitions())))
	}
	return c
}

// NotTerminal reports if the current state has no outgoing edges — a machine that
// can never move again, where the test expects it still can.
func (c *Checker[S]) NotTerminal() *Checker[S] {
	c.tb.Helper()
	if c.sm.IsTerminal() {
		c.tb.Errorf("state %v is terminal, want it to have outgoing edges", c.sm.CurrentState())
	}
	return c
}

// Allows reports unless the states reachable from the current one are exactly want,
// in any order. It asserts on the graph as declared, saying nothing about whether
// those transitions would succeed.
func (c *Checker[S]) Allows(want ...S) *Checker[S] {
	c.tb.Helper()
	got := slices.Collect(c.sm.AllowedTransitions())
	if len(got) != len(want) {
		c.tb.Errorf("%v allows %s, want %s", c.sm.CurrentState(), render(got), render(want))
		return c
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			c.tb.Errorf("%v allows %s, want %s", c.sm.CurrentState(), render(got), render(want))
			return c
		}
	}
	return c
}

// Reachable reports every declared state that cannot be reached from the current
// one by following declared edges — a state the graph can never enter, which is
// almost always a typo in a [statemachine.StateDef.To] list.
//
// Call it on a freshly built machine, where the current state is the initial one;
// from a terminal state nothing is reachable and everything is reported.
func (c *Checker[S]) Reachable() *Checker[S] {
	c.tb.Helper()

	edges := make(map[S][]S)
	for state, def := range c.sm.States() {
		edges[state] = def.To
	}

	start := c.sm.CurrentState()
	seen := map[S]struct{}{start: {}}
	for queue := []S{start}; len(queue) > 0; {
		state := queue[0]
		queue = queue[1:]
		for _, to := range edges[state] {
			if _, ok := seen[to]; ok {
				continue
			}
			seen[to] = struct{}{}
			queue = append(queue, to)
		}
	}

	var unreachable []S
	for state := range edges {
		if _, ok := seen[state]; !ok {
			unreachable = append(unreachable, state)
		}
	}
	if len(unreachable) > 0 {
		// Map order is unspecified, so sort by rendering to keep the failure stable.
		slices.SortFunc(unreachable, func(a, b S) int {
			return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
		})
		c.tb.Errorf("state(s) %s cannot be reached from %v", render(unreachable), start)
	}
	return c
}

// NoDeadline reports if the current state has a deadline.
func (c *Checker[S]) NoDeadline() *Checker[S] {
	c.tb.Helper()
	if deadline, ok := c.sm.Deadline(); ok {
		c.tb.Errorf("state %v expires in %v, want no deadline",
			c.sm.CurrentState(), time.Until(deadline).Round(time.Millisecond))
	}
	return c
}

// DeadlineIn reports unless the current state expires want from now, give or take
// tolerance. The tolerance is what makes this usable: a deadline is armed when the
// entry hook returns, so by the time a test looks at it some of it has already
// elapsed.
func (c *Checker[S]) DeadlineIn(want, tolerance time.Duration) *Checker[S] {
	c.tb.Helper()
	deadline, ok := c.sm.Deadline()
	if !ok {
		c.tb.Errorf("state %v has no deadline, want one in ~%v", c.sm.CurrentState(), want)
		return c
	}
	remaining := time.Until(deadline)
	if remaining < want-tolerance || remaining > want+tolerance {
		c.tb.Errorf("state %v expires in %v, want %v ± %v",
			c.sm.CurrentState(), remaining.Round(time.Millisecond), want, tolerance)
	}
	return c
}

// TimedOut reports unless the current state's deadline has passed. See [Expire] for
// getting there without waiting.
func (c *Checker[S]) TimedOut() *Checker[S] {
	c.tb.Helper()
	if !c.sm.TimeoutReached() {
		c.tb.Errorf("state %v has not timed out (%s)", c.sm.CurrentState(), c.deadlineDesc())
	}
	return c
}

// NotTimedOut reports if the current state's deadline has passed.
func (c *Checker[S]) NotTimedOut() *Checker[S] {
	c.tb.Helper()
	if c.sm.TimeoutReached() {
		c.tb.Errorf("state %v has timed out, want it still within its bound", c.sm.CurrentState())
	}
	return c
}

// Enters transitions the machine to state to and fails the test if that does not
// work, for the transitions a test needs to make rather than the ones it is
// asserting about. Use [Checker.Refuses] for an edge expected to be rejected.
func (c *Checker[S]) Enters(ctx context.Context, to S) *Checker[S] {
	c.tb.Helper()
	from := c.sm.CurrentState()
	if err := c.sm.TransitionTo(ctx, to); err != nil {
		c.tb.Fatalf("transition %v -> %v: %v", from, to, err)
	}
	return c
}

// Refuses reports unless the graph rejects the edge from the current state to to
// with a [statemachine.IllegalTransitionError] naming both endpoints. Any other
// error is a failure too: a hook that ran means the edge was allowed after all.
//
// It fails fatally on a transition that *succeeded*, since the machine has then
// moved somewhere the rest of the test does not expect.
func (c *Checker[S]) Refuses(ctx context.Context, to S) *Checker[S] {
	c.tb.Helper()
	from := c.sm.CurrentState()

	err := c.sm.TransitionTo(ctx, to)
	if err == nil {
		c.tb.Fatalf("transition %v -> %v succeeded, want it refused by the graph", from, to)
	}

	illegal, ok := errors.AsType[*statemachine.IllegalTransitionError[S]](err)
	switch {
	case !ok:
		c.tb.Errorf("transition %v -> %v failed with %v, want an IllegalTransitionError", from, to, err)
	case illegal.From != from || illegal.To != to:
		c.tb.Errorf("transition %v -> %v reported the edge %v -> %v", from, to, illegal.From, illegal.To)
	}
	if got := c.sm.CurrentState(); got != from {
		c.tb.Errorf("machine moved to %v on a refused transition, want it left in %v", got, from)
	}
	return c
}

// deadlineDesc describes the current state's deadline for a failure message.
func (c *Checker[S]) deadlineDesc() string {
	deadline, ok := c.sm.Deadline()
	if !ok {
		return "it has no deadline"
	}
	return fmt.Sprintf("it expires in %v", time.Until(deadline).Round(time.Millisecond))
}

// Expire backdates the current state's deadline so that the machine reports a
// timeout immediately, letting a test exercise the timeout path of a state bounded
// by minutes without spending them.
//
// It goes through [statemachine.Machine.Restore], so like a restore it runs no
// entry hook and validates no edge — the machine stays in the state it is in, with
// a deadline in the past. A state that had no deadline gets one, since a test
// asking for a timeout wants one either way.
func Expire[S comparable](tb testing.TB, sm *statemachine.Machine[S]) {
	tb.Helper()
	state := sm.CurrentState()
	// One second, not one nanosecond: a deadline within the clock's resolution of
	// now is not reliably in the past.
	snap := statemachine.Snapshot[S]{State: state, Deadline: time.Now().Add(-time.Second)}
	if err := sm.Restore(snap); err != nil {
		tb.Fatalf("smtest.Expire: expiring %v: %v", state, err)
	}
}

// render formats a list of states for a failure message.
func render[S comparable](states []S) string {
	if len(states) == 0 {
		return "[]"
	}
	parts := make([]string, len(states))
	for i, s := range states {
		parts[i] = fmt.Sprint(s)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
