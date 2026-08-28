package statemachine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// sub is the sub-phase type shared by every action of the fixture, mirroring a
// StorageNodeOps whose single subPhase enum is the union of several workflows'
// steps.
type sub string

const (
	// The remove workflow.
	validating sub = "Validating"
	suspending sub = "Suspending"
	removed    sub = "Removed"

	// The migrate workflow, which shares no state with remove.
	preparing sub = "Preparing"
	promoted  sub = "Promoted"

	// undeclaredSub is never a key of any graph in the fixture.
	undeclaredSub sub = "Nope"
)

const (
	remove  Action = "remove"
	migrate Action = "migrate"
	// shutdown is an action the fixture deliberately declares no graph for,
	// standing in for the StorageNodeOps actions that have no sub-phases at all.
	shutdown Action = "shutdown"
)

// subHook is hook for the sub state type.
type subHook struct {
	calls   int
	from    sub
	to      sub
	timeout time.Duration
	err     error
}

func (h *subHook) fn(_ context.Context, from, to sub) (time.Duration, error) {
	h.calls++
	h.from, h.to = from, to
	return h.timeout, h.err
}

// multiActions is the canonical two-workflow fixture:
//
//	remove:  Validating -> Suspending -> Removed
//	migrate: Preparing  -> Promoted
//
// The workflows share the state type and no state, which is what makes a step of
// one illegal in the other.
func multiActions(onSuspending, onPreparing TransitionFunc[sub]) MultiConfig[sub] {
	return MultiConfig[sub]{
		remove: {
			Initial: validating,
			States: map[sub]StateDef[sub]{
				validating: {To: []sub{suspending}},
				suspending: {To: []sub{removed}, OnEnter: onSuspending},
				removed:    {},
			},
		},
		migrate: {
			Initial: preparing,
			States: map[sub]StateDef[sub]{
				preparing: {To: []sub{promoted}, OnEnter: onPreparing},
				promoted:  {},
			},
		},
	}
}

func newMulti(t *testing.T, mc MultiConfig[sub], action Action) *Machine[sub] {
	t.Helper()
	sm, err := mc.New(context.Background(), action)
	if err != nil {
		t.Fatalf("MultiConfig.New(%v): %v", action, err)
	}
	t.Cleanup(sm.Close)
	return sm
}

// --- selecting an action's graph ----------------------------------------------

func TestMultiConfigNewSelectsTheActionsGraph(t *testing.T) {
	mc := multiActions(nil, nil)

	rm := newMulti(t, mc, remove)
	if got := rm.CurrentState(); got != validating {
		t.Errorf("remove starts in %v, want %v", got, validating)
	}
	if !rm.CanTransitionTo(suspending) {
		t.Error("remove cannot reach its own second step")
	}

	mg := newMulti(t, mc, migrate)
	if got := mg.CurrentState(); got != preparing {
		t.Errorf("migrate starts in %v, want %v", got, preparing)
	}
	if !mg.CanTransitionTo(promoted) {
		t.Error("migrate cannot reach its own second step")
	}
}

func TestMultiConfigIsolatesActions(t *testing.T) {
	// The point of the whole type: the CRD's subPhase enum is a union, so nothing
	// but the graph stops a remove op from landing in a migrate step.
	rm := newMulti(t, multiActions(nil, nil), remove)

	err := rm.TransitionTo(context.Background(), preparing)
	illegal, ok := errors.AsType[*IllegalTransitionError[sub]](err)
	if !ok {
		t.Fatalf("err = %v, want IllegalTransitionError", err)
	}
	if illegal.From != validating || illegal.To != preparing {
		t.Errorf("error carries %v -> %v, want %v -> %v", illegal.From, illegal.To, validating, preparing)
	}
	if rm.CurrentState() != validating {
		t.Errorf("a rejected transition moved the machine to %v", rm.CurrentState())
	}
}

func TestMultiConfigNewRejectsUnknownAction(t *testing.T) {
	// An action with no graph is user input, not a programmer error: spec.action
	// survives a downgrade and a hand-edited resource.
	sm, err := multiActions(nil, nil).New(context.Background(), shutdown)
	if !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("err = %v, want ErrUnknownAction", err)
	}
	if sm != nil {
		t.Error("a rejected New returned a machine")
	}
	if !strings.Contains(err.Error(), string(shutdown)) {
		t.Errorf("err = %q, want it to name the action", err)
	}
}

func TestMultiConfigEmptyRejectsEverything(t *testing.T) {
	var mc MultiConfig[sub]
	if _, err := mc.New(context.Background(), remove); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("err = %v, want ErrUnknownAction from a nil MultiConfig", err)
	}
}

func TestMultiConfigValidatesEveryGraph(t *testing.T) {
	// A switch over spec.action only ever validates the branch it takes. This type
	// exists partly so that reconciling any action proves every action sound.
	mc := multiActions(nil, nil)
	mc[migrate] = Config[sub]{
		Initial: preparing,
		States: map[sub]StateDef[sub]{
			preparing: {To: []sub{undeclaredSub}},
		},
	}

	sm, err := mc.New(context.Background(), remove)
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState from the unselected graph", err)
	}
	if sm != nil {
		t.Error("a rejected New returned a machine")
	}
}

func TestMultiConfigNewRunsNoHook(t *testing.T) {
	// A machine is born already in its initial state; New must not enter it.
	h := &subHook{timeout: time.Minute}
	mc := multiActions(nil, h.fn)

	sm := newMulti(t, mc, migrate)
	if h.calls != 0 {
		t.Errorf("New ran the initial state's hook %d times", h.calls)
	}
	if _, ok := sm.Deadline(); ok {
		t.Error("New armed a deadline")
	}
}

func TestMultiConfigMachineIsBoundByItsContext(t *testing.T) {
	// The returned machine is an ordinary one: the context passed in bounds it.
	ctx, cancel := context.WithCancel(context.Background())
	sm, err := multiActions(nil, nil).New(ctx, remove)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sm.Close()

	cancel()
	if err := sm.TransitionTo(context.Background(), suspending); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed after the parent context was canceled", err)
	}
}

// --- FromSnapshot --------------------------------------------------------------

func TestMultiConfigFromSnapshotRestoresStateAndDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Hour)
		sm, err := multiActions(nil, nil).FromSnapshot(context.Background(), remove,
			Snapshot[sub]{State: suspending, Deadline: deadline})
		if err != nil {
			t.Fatalf("FromSnapshot: %v", err)
		}
		defer sm.Close()

		if got := sm.CurrentState(); got != suspending {
			t.Errorf("CurrentState() = %v, want %v", got, suspending)
		}
		got, ok := sm.Deadline()
		if !ok {
			t.Fatal("FromSnapshot dropped the deadline")
		}
		if !got.Equal(deadline) {
			t.Errorf("Deadline() = %v, want %v", got, deadline)
		}
		// The restored machine still has the selected action's edges.
		if !sm.CanTransitionTo(removed) {
			t.Error("restored machine lost its outgoing edges")
		}
	})
}

func TestMultiConfigFromSnapshotZeroStateStartsAtInitial(t *testing.T) {
	// The reason FromSnapshot exists: it absorbs the caller's
	// `if op.Status.SubPhase != ""` guard. An empty subPhase is not a state, it
	// means nobody has reconciled this resource yet.
	sm, err := multiActions(nil, nil).FromSnapshot(context.Background(), migrate, Snapshot[sub]{})
	if err != nil {
		t.Fatalf("FromSnapshot: %v", err)
	}
	defer sm.Close()

	if got := sm.CurrentState(); got != preparing {
		t.Errorf("CurrentState() = %v, want the initial state %v", got, preparing)
	}
	if _, ok := sm.Deadline(); ok {
		t.Error("a fresh machine came back with a deadline")
	}
}

func TestMultiConfigFromSnapshotZeroStateIgnoresDeadline(t *testing.T) {
	// A deadline without a state is incoherent input; the state field decides.
	synctest.Test(t, func(t *testing.T) {
		sm, err := multiActions(nil, nil).FromSnapshot(context.Background(), migrate,
			Snapshot[sub]{Deadline: time.Now().Add(-time.Hour)})
		if err != nil {
			t.Fatalf("FromSnapshot: %v", err)
		}
		defer sm.Close()

		if _, ok := sm.Deadline(); ok {
			t.Error("a stateless snapshot armed its deadline")
		}
		if sm.TimeoutReached() {
			t.Error("a fresh machine reported a timeout")
		}
	})
}

func TestMultiConfigFromSnapshotRestoresADeclaredZeroState(t *testing.T) {
	// "Zero means fresh" is a rule about undeclared states only. For an int-backed
	// phase whose zero value is a real state, the snapshot is restored as written
	// — deadline included, which is what tells this apart from starting over.
	type phase int
	const (
		start phase = iota
		done
	)
	mc := MultiConfig[phase]{
		remove: {
			Initial: done,
			States: map[phase]StateDef[phase]{
				start: {To: []phase{done}},
				done:  {},
			},
		},
	}

	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Hour)
		sm, err := mc.FromSnapshot(context.Background(), remove, Snapshot[phase]{Deadline: deadline})
		if err != nil {
			t.Fatalf("FromSnapshot: %v", err)
		}
		defer sm.Close()

		if got := sm.CurrentState(); got != start {
			t.Errorf("CurrentState() = %v, want the declared zero state %v", got, start)
		}
		got, ok := sm.Deadline()
		if !ok {
			t.Fatal("a declared zero state was treated as a fresh machine")
		}
		if !got.Equal(deadline) {
			t.Errorf("Deadline() = %v, want %v", got, deadline)
		}
	})
}

func TestMultiConfigFromSnapshotRejectsUnknownState(t *testing.T) {
	// A downgrade, or a hand-edited resource. It must stay loud rather than
	// silently starting the workflow over.
	sm, err := multiActions(nil, nil).FromSnapshot(context.Background(), remove,
		Snapshot[sub]{State: undeclaredSub})
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState", err)
	}
	if sm != nil {
		t.Error("a rejected FromSnapshot returned a machine")
	}
}

func TestMultiConfigFromSnapshotRejectsAnotherActionsState(t *testing.T) {
	// The union enum again: Preparing is a declared state of the type, and of the
	// migrate graph, but it is not a state a remove op can be in.
	_, err := multiActions(nil, nil).FromSnapshot(context.Background(), remove,
		Snapshot[sub]{State: preparing})
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState for a foreign action's state", err)
	}
}

func TestMultiConfigFromSnapshotRejectsUnknownAction(t *testing.T) {
	sm, err := multiActions(nil, nil).FromSnapshot(context.Background(), shutdown,
		Snapshot[sub]{State: validating})
	if !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("err = %v, want ErrUnknownAction", err)
	}
	if sm != nil {
		t.Error("a rejected FromSnapshot returned a machine")
	}
}

func TestMultiConfigFromSnapshotRunsNoHook(t *testing.T) {
	// The transition into that state already happened, in an earlier process.
	h := &subHook{timeout: time.Minute}
	sm, err := multiActions(h.fn, nil).FromSnapshot(context.Background(), remove,
		Snapshot[sub]{State: suspending})
	if err != nil {
		t.Fatalf("FromSnapshot: %v", err)
	}
	defer sm.Close()

	if h.calls != 0 {
		t.Errorf("FromSnapshot ran the entry hook %d times", h.calls)
	}
	if _, ok := sm.Deadline(); ok {
		t.Error("FromSnapshot armed the hook's deadline instead of the snapshot's")
	}
}

func TestMultiConfigFromSnapshotExpiredDeadlineFiresImmediately(t *testing.T) {
	// The operator was down while the sub-phase ran out of time.
	synctest.Test(t, func(t *testing.T) {
		sm, err := multiActions(nil, nil).FromSnapshot(context.Background(), remove,
			Snapshot[sub]{State: suspending, Deadline: time.Now().Add(-time.Hour)})
		if err != nil {
			t.Fatalf("FromSnapshot: %v", err)
		}
		defer sm.Close()

		if !sm.TimeoutReached() {
			t.Fatal("a lapsed deadline did not report as reached")
		}
		if d, ok := sm.RequeueAfter(); ok {
			t.Errorf("RequeueAfter() = %v, true for a lapsed deadline", d)
		}
	})
}

func TestMultiConfigFromSnapshotValidatesEveryGraph(t *testing.T) {
	mc := multiActions(nil, nil)
	mc[migrate] = Config[sub]{
		Initial: preparing,
		States: map[sub]StateDef[sub]{
			preparing: {To: []sub{undeclaredSub}},
		},
	}

	_, err := mc.FromSnapshot(context.Background(), remove, Snapshot[sub]{State: validating})
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState from the unselected graph", err)
	}
}
