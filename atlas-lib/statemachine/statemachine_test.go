package statemachine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// st is the string-backed state type used by most tests, mirroring how a
// controller would key a CR's subPhase field.
type st string

const (
	off  st = "off"
	on   st = "on"
	rdy  st = "ready"
	term st = "terminal"
	// undeclared is never a key of the test graph.
	undeclared st = "nope"
)

// hook builds a TransitionFunc that records its arguments and returns a fixed
// result, so tests can assert on what the machine passed it.
type hook struct {
	calls   int
	from    st
	to      st
	ctx     context.Context
	timeout time.Duration
	err     error
}

func (h *hook) fn(ctx context.Context, from, to st) (time.Duration, error) {
	h.calls++
	h.from, h.to, h.ctx = from, to, ctx
	return h.timeout, h.err
}

// graph is the canonical test machine:
//
//	off -> off, on      (no hook)
//	on  -> off, ready   (hook, supplied per test)
//	ready -> off, terminal, ready
//	terminal            (declared, no exits)
func graph(onHook TransitionFunc[st]) Config[st] {
	return Config[st]{
		Initial: off,
		States: map[st]StateDef[st]{
			off:  {To: []st{off, on}},
			on:   {To: []st{off, rdy}, OnEnter: onHook},
			rdy:  {To: []st{off, term, rdy}},
			term: {},
		},
	}
}

func newTest(t *testing.T, onHook TransitionFunc[st]) *Machine[st] {
	t.Helper()
	sm, err := New(context.Background(), graph(onHook))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { sm.Close() })
	return sm
}

// mustTransition moves the machine and fails the test if it cannot.
func mustTransition(t *testing.T, sm *Machine[st], to st) {
	t.Helper()
	if err := sm.TransitionTo(context.Background(), to); err != nil {
		t.Fatalf("TransitionTo(%v): %v", to, err)
	}
}

// arm puts the machine in `on` with a one minute deadline and returns it.
func armed(t *testing.T) *Machine[st] {
	t.Helper()
	sm := newTest(t, (&hook{timeout: time.Minute}).fn)
	mustTransition(t, sm, on)
	if _, ok := sm.Deadline(); !ok {
		t.Fatal("setup: expected an armed deadline")
	}
	return sm
}

// --- construction and validation ---------------------------------------------

func TestNewValid(t *testing.T) {
	sm := newTest(t, nil)

	if got := sm.CurrentState(); got != off {
		t.Errorf("CurrentState() = %v, want %v", got, off)
	}
	if sm.Context() == nil {
		t.Fatal("Context() is nil")
	}
	if err := sm.Context().Err(); err != nil {
		t.Errorf("state context already done: %v", err)
	}
	if _, ok := sm.Deadline(); ok {
		t.Error("a fresh machine should have no deadline")
	}
	if sm.TimeoutReached() {
		t.Error("a fresh machine should not report a timeout")
	}
	if _, ok := sm.RequeueAfter(); ok {
		t.Error("a fresh machine should not ask to be requeued")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := map[string]Config[st]{
		"initial state not declared": {
			Initial: off,
			States:  map[st]StateDef[st]{on: {}},
		},
		"nil states map": {
			Initial: off,
		},
		"empty states map": {
			Initial: off,
			States:  map[st]StateDef[st]{},
		},
		"edge into nowhere": {
			Initial: off,
			States:  map[st]StateDef[st]{off: {To: []st{undeclared}}},
		},
		"edge into nowhere from an unreachable state": {
			Initial: off,
			States: map[st]StateDef[st]{
				off: {},
				on:  {To: []st{undeclared}},
			},
		},
	}

	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			sm, err := New(context.Background(), cfg)
			if !errors.Is(err, ErrUnknownState) {
				t.Fatalf("err = %v, want ErrUnknownState", err)
			}
			if sm != nil {
				t.Error("a failed New should not return a machine")
			}
		})
	}
}

func TestNewAcceptsTerminalAndSelfEdges(t *testing.T) {
	// A nil To and an empty To are both terminal; a self-edge is legal.
	sm, err := New(context.Background(), Config[st]{
		Initial: off,
		States: map[st]StateDef[st]{
			off:  {To: []st{off, on, term}},
			on:   {To: []st{}},
			term: {To: nil},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sm.Close()

	if !sm.CanTransitionTo(off) {
		t.Error("self-edge should be allowed")
	}
	mustTransition(t, sm, term)
	if got := slices.Collect(sm.AllowedTransitions()); len(got) != 0 {
		t.Errorf("terminal state allows %v", got)
	}
}

func TestMust(t *testing.T) {
	t.Run("valid config returns a machine", func(t *testing.T) {
		sm := Must(context.Background(), graph(nil))
		defer sm.Close()
		if sm.CurrentState() != off {
			t.Errorf("CurrentState() = %v", sm.CurrentState())
		}
	})

	t.Run("invalid config panics", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("Must did not panic")
			}
			err, ok := r.(error)
			if !ok || !errors.Is(err, ErrUnknownState) {
				t.Errorf("panic value = %v, want an ErrUnknownState error", r)
			}
		}()
		Must(context.Background(), Config[st]{Initial: undeclared})
	})
}

func TestConfigIsDeeplyCopied(t *testing.T) {
	cfg := graph(nil)

	// Keep a reference to the exact To slice New will clone. It has to be taken
	// before New, and before the map entry holding it is replaced below —
	// reaching for cfg.States[off].To afterwards would find a different slice.
	offEdges := cfg.States[off].To
	if len(offEdges) == 0 || offEdges[0] != off {
		t.Fatalf("setup: off's edges are %v, expected them to start with %v", offEdges, off)
	}

	sm, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sm.Close()

	// Mutate every part of the config the machine might have aliased: the slice
	// itself, the map entry, the map, and the initial state.
	offEdges[0] = undeclared
	cfg.States[off] = StateDef[st]{To: []st{term}}
	delete(cfg.States, on)
	cfg.Initial = term

	if !sm.CanTransitionTo(off) {
		t.Error("machine aliased a To slice: writing through it removed an edge")
	}
	if sm.CanTransitionTo(undeclared) {
		t.Error("machine aliased a To slice: writing through it added an edge")
	}
	if !sm.CanTransitionTo(on) {
		t.Error("machine followed a later deletion from Config.States")
	}
	if sm.CanTransitionTo(term) {
		t.Error("machine picked up an edge added after New")
	}
	sm.Reset()
	if sm.CurrentState() != off {
		t.Errorf("machine followed a later mutation of Config.Initial: %v", sm.CurrentState())
	}
}

func TestNonStringStateType(t *testing.T) {
	// The machine is generic over any comparable; ints and structs are as valid
	// as strings, and the zero value is a perfectly good state.
	type phase int
	const (
		start phase = iota
		middle
		end
	)

	sm, err := New(context.Background(), Config[phase]{
		Initial: start,
		States: map[phase]StateDef[phase]{
			start:  {To: []phase{middle}},
			middle: {To: []phase{end}},
			end:    {},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sm.Close()

	if err := sm.TransitionTo(context.Background(), middle); err != nil {
		t.Fatal(err)
	}
	err = sm.TransitionTo(context.Background(), start)
	illegal, ok := errors.AsType[*IllegalTransitionError[phase]](err)
	if !ok {
		t.Fatalf("err = %v, want IllegalTransitionError", err)
	}
	if illegal.From != middle || illegal.To != start {
		t.Errorf("error carries %v -> %v", illegal.From, illegal.To)
	}
}

// --- transitions --------------------------------------------------------------

func TestTransitionRunsHookWithBothEndpoints(t *testing.T) {
	h := &hook{}
	sm := newTest(t, h.fn)

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller")

	if err := sm.TransitionTo(ctx, on); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	if h.calls != 1 {
		t.Fatalf("hook called %d times, want 1", h.calls)
	}
	if h.from != off || h.to != on {
		t.Errorf("hook got from=%v to=%v, want from=%v to=%v", h.from, h.to, off, on)
	}
	if got := h.ctx.Value(ctxKey{}); got != "caller" {
		t.Errorf("hook got ctx value %v, want the caller's context", got)
	}
	if sm.CurrentState() != on {
		t.Errorf("CurrentState() = %v, want %v", sm.CurrentState(), on)
	}
}

func TestTransitionWithoutHook(t *testing.T) {
	sm := newTest(t, nil)
	// off has no hook at all; entering it must still succeed.
	mustTransition(t, sm, off)
	if sm.CurrentState() != off {
		t.Errorf("CurrentState() = %v", sm.CurrentState())
	}
}

func TestIllegalTransition(t *testing.T) {
	sm := armed(t)
	deadlineBefore, _ := sm.Deadline()
	ctxBefore := sm.Context()

	err := sm.TransitionTo(context.Background(), term)

	illegal, ok := errors.AsType[*IllegalTransitionError[st]](err)
	if !ok {
		t.Fatalf("err = %v (%T), want IllegalTransitionError", err, err)
	}
	if illegal.From != on || illegal.To != term {
		t.Errorf("error carries %v -> %v, want %v -> %v", illegal.From, illegal.To, on, term)
	}
	if want := "statemachine: illegal transition on -> terminal"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	// Nothing about the machine may have moved.
	if sm.CurrentState() != on {
		t.Errorf("CurrentState() = %v, want %v", sm.CurrentState(), on)
	}
	if got, _ := sm.Deadline(); !got.Equal(deadlineBefore) {
		t.Error("deadline changed on a rejected transition")
	}
	if sm.Context() != ctxBefore {
		t.Error("state context was replaced on a rejected transition")
	}
	if err := ctxBefore.Err(); err != nil {
		t.Errorf("state context cancelled on a rejected transition: %v", err)
	}
}

func TestTerminalStateRejectsEverything(t *testing.T) {
	sm := newTest(t, nil)
	mustTransition(t, sm, on)
	mustTransition(t, sm, rdy)
	mustTransition(t, sm, term)

	for _, to := range []st{off, on, rdy, term} {
		err := sm.TransitionTo(context.Background(), to)
		if _, ok := errors.AsType[*IllegalTransitionError[st]](err); !ok {
			t.Errorf("TransitionTo(%v) from terminal = %v, want IllegalTransitionError", to, err)
		}
	}
	if sm.CanTransitionTo(off) {
		t.Error("CanTransitionTo from a terminal state")
	}
}

func TestSelfTransitionRerunsHookAndRearms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &hook{timeout: time.Minute}
		sm, err := New(context.Background(), Config[st]{
			Initial: on,
			States: map[st]StateDef[st]{
				on:  {To: []st{on, off}, OnEnter: h.fn},
				off: {},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer sm.Close()

		mustTransition(t, sm, on)
		first, _ := sm.Deadline()
		if h.calls != 1 || h.from != on || h.to != on {
			t.Fatalf("hook calls=%d from=%v to=%v", h.calls, h.from, h.to)
		}

		time.Sleep(30 * time.Second)
		mustTransition(t, sm, on)

		second, _ := sm.Deadline()
		if h.calls != 2 {
			t.Errorf("hook called %d times, want 2", h.calls)
		}
		if !second.After(first) {
			t.Errorf("self-transition did not re-arm: %v then %v", first, second)
		}
	})
}

func TestHookErrorLeavesMachineUntouched(t *testing.T) {
	sentinel := errors.New("hardware said no")
	h := &hook{timeout: time.Hour, err: sentinel}
	sm := newTest(t, h.fn)

	// The hook asks for an hour and then fails; neither may take effect.
	err := sm.TransitionTo(context.Background(), on)

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, does not wrap the hook's error", err)
	}
	if _, ok := errors.AsType[*IllegalTransitionError[st]](err); ok {
		t.Error("a hook failure must not look like an illegal transition")
	}
	if got, want := err.Error(), "statemachine: entering on from off: hardware said no"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if sm.CurrentState() != off {
		t.Errorf("CurrentState() = %v, want %v", sm.CurrentState(), off)
	}
	if _, ok := sm.Deadline(); ok {
		t.Error("a failed hook armed a deadline anyway")
	}
}

func TestHookErrorPreservesExistingDeadline(t *testing.T) {
	// A failed transition must not disturb the deadline of the state the machine
	// is stuck in: off -> on arms a minute, then on -> ready fails.
	sm, err := New(context.Background(), Config[st]{
		Initial: off,
		States: map[st]StateDef[st]{
			off: {To: []st{on}},
			on:  {To: []st{rdy}, OnEnter: (&hook{timeout: time.Minute}).fn},
			rdy: {OnEnter: (&hook{err: errors.New("nope")}).fn},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sm.Close()

	mustTransition(t, sm, on)
	armedAt, ok := sm.Deadline()
	if !ok {
		t.Fatal("setup: on did not arm a deadline")
	}
	stateCtx := sm.Context()

	if err := sm.TransitionTo(context.Background(), rdy); err == nil {
		t.Fatal("expected ready's hook to fail")
	}

	if sm.CurrentState() != on {
		t.Errorf("CurrentState() = %v, want %v", sm.CurrentState(), on)
	}
	got, ok := sm.Deadline()
	if !ok {
		t.Fatal("the failed transition cleared the existing deadline")
	}
	if !got.Equal(armedAt) {
		t.Errorf("deadline moved from %v to %v", armedAt, got)
	}
	if err := stateCtx.Err(); err != nil {
		t.Errorf("the failed transition cancelled the live state context: %v", err)
	}
}

func TestNilHookClearsDeadline(t *testing.T) {
	sm := armed(t)
	// rdy has no hook, so entering it must leave the machine with no deadline.
	mustTransition(t, sm, rdy)
	if _, ok := sm.Deadline(); ok {
		t.Error("entering a state without a hook left a deadline armed")
	}
	if sm.TimeoutReached() {
		t.Error("TimeoutReached after entering a state without a deadline")
	}
}

func TestNonPositiveTimeoutMeansNoDeadline(t *testing.T) {
	for name, d := range map[string]time.Duration{
		"zero":     0,
		"negative": -time.Minute,
	} {
		t.Run(name, func(t *testing.T) {
			sm := newTest(t, (&hook{timeout: d}).fn)
			mustTransition(t, sm, on)
			if _, ok := sm.Deadline(); ok {
				t.Errorf("timeout %v armed a deadline", d)
			}
			if sm.TimeoutReached() {
				t.Errorf("timeout %v reported as already reached", d)
			}
		})
	}
}

func TestTransitionCancelsPreviousStateContext(t *testing.T) {
	sm := newTest(t, nil)
	first := sm.Context()

	mustTransition(t, sm, on)
	second := sm.Context()

	if !errors.Is(first.Err(), context.Canceled) {
		t.Errorf("previous state context not cancelled: %v", first.Err())
	}
	if second == first {
		t.Fatal("state context was not replaced")
	}
	if err := second.Err(); err != nil {
		t.Errorf("new state context already done: %v", err)
	}
}

func TestUnknownCurrentState(t *testing.T) {
	// Only reachable by corrupting the machine, which is what the error is for.
	sm := newTest(t, nil)
	sm.current = undeclared

	err := sm.TransitionTo(context.Background(), on)
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState", err)
	}
	if _, ok := errors.AsType[*IllegalTransitionError[st]](err); ok {
		t.Error("a corrupt current state must not report as an illegal transition")
	}
}

// --- reentrancy ---------------------------------------------------------------

func TestReentrantTransitionRejected(t *testing.T) {
	sm := newTest(t, nil)

	var inner error
	var innerState st
	h := &hook{timeout: time.Minute}
	sm.states[on] = StateDef[st]{
		To: []st{off, rdy},
		OnEnter: func(ctx context.Context, from, to st) (time.Duration, error) {
			inner = sm.TransitionTo(ctx, rdy)
			innerState = sm.CurrentState()
			return h.fn(ctx, from, to)
		},
	}

	if err := sm.TransitionTo(context.Background(), on); err != nil {
		t.Fatalf("outer TransitionTo: %v", err)
	}

	if !errors.Is(inner, ErrReentrantTransition) {
		t.Fatalf("nested TransitionTo = %v, want ErrReentrantTransition", inner)
	}
	// The nested call must not have moved anything, and the outer call must
	// still have applied its own result.
	if innerState != off {
		t.Errorf("nested call changed the state to %v mid-transition", innerState)
	}
	if sm.CurrentState() != on {
		t.Errorf("CurrentState() = %v, want %v", sm.CurrentState(), on)
	}
	if _, ok := sm.Deadline(); !ok {
		t.Error("outer transition lost its deadline")
	}
}

func TestReentrantRestoreRejected(t *testing.T) {
	sm := newTest(t, nil)

	var inner error
	sm.states[on] = StateDef[st]{
		To: []st{off},
		OnEnter: func(ctx context.Context, from, to st) (time.Duration, error) {
			inner = sm.Restore(Snapshot[st]{State: rdy})
			return 0, nil
		},
	}

	mustTransition(t, sm, on)
	if !errors.Is(inner, ErrReentrantTransition) {
		t.Fatalf("nested Restore = %v, want ErrReentrantTransition", inner)
	}
	if sm.CurrentState() != on {
		t.Errorf("CurrentState() = %v, want %v", sm.CurrentState(), on)
	}
}

func TestGuardReleasedAfterFailedHook(t *testing.T) {
	sm := newTest(t, (&hook{err: errors.New("boom")}).fn)

	if err := sm.TransitionTo(context.Background(), on); err == nil {
		t.Fatal("expected the hook to fail")
	}
	// The machine must not be wedged.
	if err := sm.TransitionTo(context.Background(), off); err != nil {
		t.Fatalf("machine wedged after a failed hook: %v", err)
	}
}

func TestGuardReleasedAfterPanickingHook(t *testing.T) {
	sm := newTest(t, func(context.Context, st, st) (time.Duration, error) {
		panic("hook exploded")
	})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic did not propagate to the caller")
			}
		}()
		_ = sm.TransitionTo(context.Background(), on)
	}()

	if sm.entering != nil {
		t.Fatal("reentrancy guard still raised after a panicking hook")
	}
	if err := sm.TransitionTo(context.Background(), off); err != nil {
		t.Fatalf("machine wedged after a panicking hook: %v", err)
	}
}

// --- deadlines ----------------------------------------------------------------

func TestDeadlineValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		sm := newTest(t, (&hook{timeout: time.Minute}).fn)
		mustTransition(t, sm, on)

		deadline, ok := sm.Deadline()
		if !ok {
			t.Fatal("no deadline armed")
		}
		if want := start.Add(time.Minute); !deadline.Equal(want) {
			t.Errorf("Deadline() = %v, want %v", deadline, want)
		}
	})
}

func TestDeadlineClockStartsWhenHookReturns(t *testing.T) {
	// A documented semantic: a hook that blocks for 20s and then asks for 60s
	// yields a state that expires 80s after the caller asked for it.
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		sm := newTest(t, func(context.Context, st, st) (time.Duration, error) {
			time.Sleep(20 * time.Second)
			return time.Minute, nil
		})
		mustTransition(t, sm, on)

		deadline, ok := sm.Deadline()
		if !ok {
			t.Fatal("no deadline armed")
		}
		if want := start.Add(80 * time.Second); !deadline.Equal(want) {
			t.Errorf("Deadline() = %v, want %v (start + 20s hook + 60s)", deadline, want)
		}
	})
}

func TestTimeoutFiresAndIsAcknowledged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sm := armed(t)
		stateCtx := sm.Context()

		time.Sleep(59 * time.Second)
		synctest.Wait()
		if sm.TimeoutReached() {
			t.Fatal("timeout fired early")
		}
		if _, ok := sm.RequeueAfter(); !ok {
			t.Error("RequeueAfter should still be pending")
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()

		if !sm.TimeoutReached() {
			t.Fatal("timeout did not fire")
		}
		if !errors.Is(stateCtx.Err(), context.DeadlineExceeded) {
			t.Errorf("state context err = %v, want DeadlineExceeded", stateCtx.Err())
		}
		if sm.CurrentState() != on {
			t.Errorf("a timeout changed the state to %v", sm.CurrentState())
		}
		if d, ok := sm.RequeueAfter(); ok {
			t.Errorf("RequeueAfter() = %v, true for an expired state", d)
		}
		// It keeps reporting until acknowledged.
		if !sm.TimeoutReached() {
			t.Fatal("TimeoutReached stopped reporting without an acknowledgement")
		}

		sm.ClearTimeout()

		if sm.TimeoutReached() {
			t.Error("ClearTimeout did not acknowledge the timeout")
		}
		if _, ok := sm.Deadline(); ok {
			t.Error("ClearTimeout left a deadline armed")
		}
		if sm.CurrentState() != on {
			t.Errorf("ClearTimeout changed the state to %v", sm.CurrentState())
		}
	})
}

func TestClearTimeoutWithoutDeadline(t *testing.T) {
	sm := newTest(t, nil)
	before := sm.Context()

	sm.ClearTimeout()

	// Documented: it replaces the state context either way.
	if !errors.Is(before.Err(), context.Canceled) {
		t.Errorf("previous state context not cancelled: %v", before.Err())
	}
	if sm.Context() == before {
		t.Error("state context not replaced")
	}
	if err := sm.Context().Err(); err != nil {
		t.Errorf("new state context already done: %v", err)
	}
}

func TestDoneClosesAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sm := armed(t)
		done := sm.Done()

		select {
		case <-done:
			t.Fatal("Done closed before the deadline")
		default:
		}

		// A real caller selects on this instead of polling.
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
			t.Fatal("Done never closed")
		}
		if !sm.TimeoutReached() {
			t.Error("Done closed without a timeout being reported")
		}
	})
}

func TestDoneIsReplacedPerTransition(t *testing.T) {
	sm := newTest(t, nil)
	first := sm.Done()
	mustTransition(t, sm, on)

	select {
	case <-first:
	default:
		t.Fatal("the previous state's Done channel was not closed")
	}
	select {
	case <-sm.Done():
		t.Fatal("the new state's Done channel is already closed")
	default:
	}
}

func TestCallerContextDoesNotBoundTheState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sm := newTest(t, (&hook{timeout: time.Minute}).fn)

		// The caller's own budget is far shorter than the state's.
		callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		mustTransition2(t, sm, on, callCtx)
		cancel() // the request is over; the state must not be

		if err := sm.Context().Err(); err != nil {
			t.Fatalf("the caller's cancel killed the state context: %v", err)
		}

		time.Sleep(30 * time.Second)
		synctest.Wait()
		if sm.TimeoutReached() {
			t.Fatal("the state expired on the caller's 5s budget")
		}

		time.Sleep(31 * time.Second)
		synctest.Wait()
		if !sm.TimeoutReached() {
			t.Fatal("the state did not expire on its own minute")
		}
	})
}

func mustTransition2(t *testing.T, sm *Machine[st], to st, ctx context.Context) {
	t.Helper()
	if err := sm.TransitionTo(ctx, to); err != nil {
		t.Fatalf("TransitionTo(%v): %v", to, err)
	}
}

func TestAlreadyCancelledCallerContextStillTransitions(t *testing.T) {
	// The call context bounds the hook, and this hook does not consult it, so
	// the transition succeeds. Documented behaviour, worth pinning down.
	sm := newTest(t, (&hook{}).fn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := sm.TransitionTo(ctx, on); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	if sm.CurrentState() != on {
		t.Errorf("CurrentState() = %v, want %v", sm.CurrentState(), on)
	}
}

// --- lifetime -----------------------------------------------------------------

func TestClose(t *testing.T) {
	sm := Must(context.Background(), graph(nil))
	stateCtx := sm.Context()

	sm.Close()
	sm.Close() // idempotent

	if !errors.Is(stateCtx.Err(), context.Canceled) {
		t.Errorf("state context err = %v, want Canceled", stateCtx.Err())
	}
	if sm.TimeoutReached() {
		t.Error("a closed machine must not report a timeout")
	}
	if got := sm.CurrentState(); got != off {
		t.Errorf("CurrentState() after Close = %v, want %v", got, off)
	}

	err := sm.TransitionTo(context.Background(), on)
	if !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it joined with context.Canceled", err)
	}
}

func TestParentCancellationClosesMachine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sm := Must(ctx, graph(nil))
	defer sm.Close()

	cancel()

	if sm.TimeoutReached() {
		t.Error("a cancelled parent must not read as a state timeout")
	}
	if err := sm.TransitionTo(context.Background(), on); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
}

func TestParentDeadlineIsNotAStateTimeout(t *testing.T) {
	// The base context expiring means the machine is done, not that the current
	// state ran out of time — the distinction TimeoutReached exists to make.
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sm := Must(ctx, graph(nil))
		defer sm.Close()

		time.Sleep(2 * time.Second)
		synctest.Wait()

		if !errors.Is(sm.Context().Err(), context.DeadlineExceeded) {
			t.Fatalf("state context err = %v", sm.Context().Err())
		}
		if sm.TimeoutReached() {
			t.Error("the parent's deadline was reported as a state timeout")
		}
		if err := sm.TransitionTo(context.Background(), on); !errors.Is(err, ErrClosed) {
			t.Errorf("err = %v, want ErrClosed", err)
		}
	})
}

func TestStateDeadlineDoesNotOutliveTheMachine(t *testing.T) {
	sm := armed(t)
	if _, ok := sm.Deadline(); !ok {
		t.Fatal("setup")
	}
	sm.Close()
	if !errors.Is(sm.Context().Err(), context.Canceled) {
		t.Errorf("Close did not cancel the armed state context: %v", sm.Context().Err())
	}
}

func TestTransitionsDoNotAccumulateLiveContexts(t *testing.T) {
	// Every transition must cancel the context it replaces, or a long-running
	// machine leaks a timer per transition off the base context.
	sm := newTest(t, (&hook{timeout: time.Hour}).fn)

	var seen []context.Context
	for range 50 {
		mustTransition(t, sm, on)
		seen = append(seen, sm.Context())
		mustTransition(t, sm, off)
		seen = append(seen, sm.Context())
	}

	live := 0
	for _, ctx := range seen {
		if ctx.Err() == nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d live state contexts after 100 transitions, want 1", live)
	}
}

func TestReset(t *testing.T) {
	t.Run("clears state and deadline", func(t *testing.T) {
		sm := armed(t)
		sm.Reset()
		if got := sm.CurrentState(); got != off {
			t.Errorf("CurrentState() = %v, want %v", got, off)
		}
		if _, ok := sm.Deadline(); ok {
			t.Error("Reset left a deadline armed")
		}
		if sm.TimeoutReached() {
			t.Error("Reset left a timeout pending")
		}
	})

	t.Run("recovers from a terminal state", func(t *testing.T) {
		sm := newTest(t, nil)
		mustTransition(t, sm, on)
		mustTransition(t, sm, rdy)
		mustTransition(t, sm, term)

		sm.Reset()

		if got := sm.CurrentState(); got != off {
			t.Errorf("CurrentState() = %v, want %v", got, off)
		}
		if err := sm.TransitionTo(context.Background(), on); err != nil {
			t.Errorf("machine still stuck after Reset: %v", err)
		}
	})

	t.Run("after Close leaves the machine closed", func(t *testing.T) {
		sm := armed(t)
		sm.Close()
		sm.Reset()
		if got := sm.CurrentState(); got != off {
			t.Errorf("CurrentState() = %v, want %v", got, off)
		}
		if err := sm.Context().Err(); err == nil {
			t.Error("Reset revived a closed machine's context")
		}
		if err := sm.TransitionTo(context.Background(), on); !errors.Is(err, ErrClosed) {
			t.Errorf("err = %v, want ErrClosed", err)
		}
	})
}

// --- snapshot and restore -----------------------------------------------------

func TestSnapshotRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sm := armed(t)
		snap := sm.Snapshot()

		if snap.State != on {
			t.Errorf("Snapshot().State = %v, want %v", snap.State, on)
		}
		if snap.Deadline.IsZero() {
			t.Fatal("Snapshot() dropped the deadline")
		}
		if want, _ := sm.Deadline(); !snap.Deadline.Equal(want) {
			t.Errorf("Snapshot().Deadline = %v, want %v", snap.Deadline, want)
		}

		// A second process, restoring from the CR.
		restored := newTest(t, nil)
		if err := restored.Restore(snap); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if got := restored.CurrentState(); got != on {
			t.Errorf("restored state = %v, want %v", got, on)
		}
		got, ok := restored.Deadline()
		if !ok {
			t.Fatal("Restore dropped the deadline")
		}
		if !got.Equal(snap.Deadline) {
			t.Errorf("restored deadline = %v, want %v", got, snap.Deadline)
		}
		if restored.TimeoutReached() {
			t.Error("a restored future deadline reports as already reached")
		}
		// The restored machine's edges come from its own config.
		if !restored.CanTransitionTo(rdy) {
			t.Error("restored machine lost its outgoing edges")
		}
	})
}

func TestSnapshotWithoutDeadline(t *testing.T) {
	sm := newTest(t, nil)
	snap := sm.Snapshot()

	if snap.State != off {
		t.Errorf("State = %v, want %v", snap.State, off)
	}
	if !snap.Deadline.IsZero() {
		t.Errorf("Deadline = %v, want the zero time", snap.Deadline)
	}

	restored := newTest(t, nil)
	if err := restored.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, ok := restored.Deadline(); ok {
		t.Error("restoring a zero deadline armed one")
	}
	if restored.TimeoutReached() {
		t.Error("restoring a zero deadline reported a timeout")
	}
}

func TestRestoreExpiredDeadlineFiresImmediately(t *testing.T) {
	// The controller was down while the phase ran out of time; it must find out
	// on its first pass back.
	synctest.Test(t, func(t *testing.T) {
		sm := newTest(t, nil)
		err := sm.Restore(Snapshot[st]{
			State:    on,
			Deadline: time.Now().Add(-time.Hour),
		})
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}

		if !sm.TimeoutReached() {
			t.Fatal("a lapsed deadline did not report as reached")
		}
		if d, ok := sm.RequeueAfter(); ok {
			t.Errorf("RequeueAfter() = %v, true for a lapsed deadline", d)
		}
		if !errors.Is(sm.Context().Err(), context.DeadlineExceeded) {
			t.Errorf("state context err = %v, want DeadlineExceeded", sm.Context().Err())
		}
		if sm.CurrentState() != on {
			t.Errorf("CurrentState() = %v, want %v", sm.CurrentState(), on)
		}
	})
}

func TestRestoreRejectsUnknownState(t *testing.T) {
	sm := armed(t)
	before := sm.CurrentState()
	beforeDeadline, _ := sm.Deadline()

	err := sm.Restore(Snapshot[st]{State: undeclared})
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState", err)
	}
	if sm.CurrentState() != before {
		t.Errorf("a rejected Restore changed the state to %v", sm.CurrentState())
	}
	if got, _ := sm.Deadline(); !got.Equal(beforeDeadline) {
		t.Error("a rejected Restore changed the deadline")
	}
}

func TestRestoreRejectsZeroStateWhenUndeclared(t *testing.T) {
	// The controller idiom: an empty subPhase is not a state, it means "fresh".
	sm := newTest(t, nil)
	if err := sm.Restore(Snapshot[st]{}); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState for an empty state", err)
	}
}

func TestRestoreRunsNoHook(t *testing.T) {
	h := &hook{timeout: time.Minute}
	sm := newTest(t, h.fn)

	if err := sm.Restore(Snapshot[st]{State: on}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if h.calls != 0 {
		t.Errorf("Restore ran the entry hook %d times", h.calls)
	}
	if _, ok := sm.Deadline(); ok {
		t.Error("Restore armed the hook's deadline instead of the snapshot's")
	}
}

func TestRestoreIgnoresGraphEdges(t *testing.T) {
	// off cannot reach terminal in one step, but a restore is not a transition.
	sm := newTest(t, nil)
	if err := sm.Restore(Snapshot[st]{State: term}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := sm.CurrentState(); got != term {
		t.Errorf("CurrentState() = %v, want %v", got, term)
	}
}

func TestRestoreReplacesStateContext(t *testing.T) {
	sm := armed(t)
	before := sm.Context()

	if err := sm.Restore(Snapshot[st]{State: rdy}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !errors.Is(before.Err(), context.Canceled) {
		t.Errorf("Restore did not cancel the replaced state context: %v", before.Err())
	}
}

func TestSnapshotJSON(t *testing.T) {
	// The struct is meant to survive a trip through a resource's status.
	snap := Snapshot[st]{State: on, Deadline: time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"state":"on","deadline":"2026-08-10T12:00:00Z"}`
	if string(data) != want {
		t.Errorf("Marshal = %s, want %s", data, want)
	}

	var back Snapshot[st]
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.State != snap.State || !back.Deadline.Equal(snap.Deadline) {
		t.Errorf("round trip = %+v, want %+v", back, snap)
	}

	// A machine with no deadline must not persist one.
	data, err = json.Marshal(Snapshot[st]{State: off})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != `{"state":"off"}` {
		t.Errorf("Marshal = %s, want the deadline omitted", data)
	}
}

func TestRequeueAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sm := armed(t)

		d, ok := sm.RequeueAfter()
		if !ok {
			t.Fatal("RequeueAfter() = false for an armed state")
		}
		if d != time.Minute {
			t.Errorf("RequeueAfter() = %v, want %v", d, time.Minute)
		}

		time.Sleep(45 * time.Second)
		d, ok = sm.RequeueAfter()
		if !ok || d != 15*time.Second {
			t.Errorf("RequeueAfter() = %v, %v, want 15s, true", d, ok)
		}

		// Exactly at the deadline is not worth a requeue.
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if d, ok := sm.RequeueAfter(); ok {
			t.Errorf("RequeueAfter() = %v, true at the deadline", d)
		}
	})
}

// --- introspection ------------------------------------------------------------

func TestCanTransitionTo(t *testing.T) {
	sm := newTest(t, nil)

	if !sm.CanTransitionTo(on) || !sm.CanTransitionTo(off) {
		t.Error("declared edges from off reported as disallowed")
	}
	if sm.CanTransitionTo(rdy) || sm.CanTransitionTo(undeclared) {
		t.Error("undeclared edges from off reported as allowed")
	}

	mustTransition(t, sm, on)
	if sm.CanTransitionTo(on) {
		t.Error("on has no self-edge")
	}
	if !sm.CanTransitionTo(rdy) {
		t.Error("on -> ready reported as disallowed")
	}
}

func TestAllowedTransitions(t *testing.T) {
	sm := newTest(t, nil)

	if got, want := slices.Collect(sm.AllowedTransitions()), []st{off, on}; !slices.Equal(got, want) {
		t.Errorf("AllowedTransitions() = %v, want %v in declaration order", got, want)
	}

	mustTransition(t, sm, on)
	if got, want := slices.Collect(sm.AllowedTransitions()), []st{off, rdy}; !slices.Equal(got, want) {
		t.Errorf("AllowedTransitions() = %v, want %v", got, want)
	}

	// Early return from the iterator must not misbehave.
	for range sm.AllowedTransitions() {
		break
	}
}

func TestStatesIteratesWholeGraph(t *testing.T) {
	sm := newTest(t, nil)

	seen := map[st]int{}
	for state, def := range sm.States() {
		seen[state] = len(def.To)
	}

	want := map[st]int{off: 2, on: 2, rdy: 3, term: 0}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("States() = %v, want %v", seen, want)
	}

	// Early return from the iterator must not misbehave.
	for range sm.States() {
		break
	}
}

// --- NewFromSnapshot -----------------------------------------------------------

func TestNewFromSnapshotRestoresStateAndDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Hour)
		sm, err := NewFromSnapshot(context.Background(), graph(nil),
			Snapshot[st]{State: on, Deadline: deadline})
		if err != nil {
			t.Fatalf("NewFromSnapshot: %v", err)
		}
		defer sm.Close()

		if got := sm.CurrentState(); got != on {
			t.Errorf("CurrentState() = %v, want %v", got, on)
		}
		got, ok := sm.Deadline()
		if !ok {
			t.Fatal("NewFromSnapshot dropped the deadline")
		}
		if !got.Equal(deadline) {
			t.Errorf("Deadline() = %v, want %v", got, deadline)
		}
		if !sm.CanTransitionTo(rdy) {
			t.Error("restored machine lost its outgoing edges")
		}
	})
}

func TestNewFromSnapshotZeroStateStartsAtInitial(t *testing.T) {
	sm, err := NewFromSnapshot(context.Background(), graph(nil), Snapshot[st]{})
	if err != nil {
		t.Fatalf("NewFromSnapshot: %v", err)
	}
	defer sm.Close()

	if got := sm.CurrentState(); got != off {
		t.Errorf("CurrentState() = %v, want the initial state %v", got, off)
	}
	if _, ok := sm.Deadline(); ok {
		t.Error("a fresh machine came back with a deadline")
	}
}

func TestNewFromSnapshotRejectsUnknownState(t *testing.T) {
	sm, err := NewFromSnapshot(context.Background(), graph(nil), Snapshot[st]{State: undeclared})
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState", err)
	}
	if sm != nil {
		t.Error("a rejected NewFromSnapshot returned a machine")
	}
}

func TestNewFromSnapshotRejectsBadConfig(t *testing.T) {
	sm, err := NewFromSnapshot(context.Background(), Config[st]{
		Initial: off,
		States:  map[st]StateDef[st]{off: {To: []st{undeclared}}},
	}, Snapshot[st]{State: off})
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState", err)
	}
	if sm != nil {
		t.Error("a rejected NewFromSnapshot returned a machine")
	}
}

func TestNewFromSnapshotRunsNoHook(t *testing.T) {
	h := &hook{timeout: time.Minute}
	sm, err := NewFromSnapshot(context.Background(), graph(h.fn), Snapshot[st]{State: on})
	if err != nil {
		t.Fatalf("NewFromSnapshot: %v", err)
	}
	defer sm.Close()

	if h.calls != 0 {
		t.Errorf("NewFromSnapshot ran the entry hook %d times", h.calls)
	}
	if _, ok := sm.Deadline(); ok {
		t.Error("NewFromSnapshot armed the hook's deadline instead of the snapshot's")
	}
}
