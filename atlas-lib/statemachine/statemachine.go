// Package statemachine provides a small, generic, deterministic finite state
// machine with per-state entry hooks and context-based deadlines.
//
// A machine is declared up front as data: every state names the states it may
// transition to, and optionally an entry hook that runs when the machine enters
// it. Declaring the graph as a map literal makes duplicated states a compile-time
// error, and an edge pointing at an undeclared state is caught by [New] rather than
// at runtime on the unhappy path.
//
// # Deadlines
//
// An entry hook may return a duration bounding how long the machine is allowed
// to remain in the state it just entered. That deadline is expressed as a
// context, so callers can select on it, and any work performed while in the state
// can be canceled by simply carrying [Machine.Context].
//
// # Lifetime
//
// A machine owns contexts and therefore must be closed. Two contexts are in play,
// and they do different jobs: the one passed to [New] bounds the machine, and
// through it every state deadline, while the one passed to
// [Machine.TransitionTo] bounds only that call's entry hook. They are
// deliberately unconnected, so that a caller's short-lived request context does
// not silently become a state's deadline.
//
// # Concurrency
//
// A Machine is not safe for concurrent use. It is meant to be driven from a
// single goroutine, typically a poll or reconcile loop. Guard it with a mutex if
// you need more. Holding that mutex across [Machine.TransitionTo] is safe,
// because a hook cannot re-enter the machine that is calling it. See
// [ErrReentrantTransition].
//
// # Example
//
// The phases of a volume migration. A VolumeMigration is submitted to the
// storage API, its new NVMe-oF paths are validated on the target node, the data
// is copied, and it ends up completed, failed, or aborted:
//
//	Pending ── Validating ── Running ── Completed
//	   └───────────┴───────────┴────── Failed
//	               └───────────┴────── Aborted
//
// The CRD's own phase type is the state type, so no parallel enum is needed.
//
//	// A type alias keeps the map literal readable.
//	type phase = v1alpha1.VolumeMigrationPhase
//
//	const (
//		pending    = v1alpha1.VolumeMigrationPhasePending
//		validating = v1alpha1.VolumeMigrationPhaseValidating
//		running    = v1alpha1.VolumeMigrationPhaseRunning
//		completed  = v1alpha1.VolumeMigrationPhaseCompleted
//		failed     = v1alpha1.VolumeMigrationPhaseFailed
//		aborted    = v1alpha1.VolumeMigrationPhaseAborted
//	)
//
//	// configFor builds the graph for one migration. The hooks close over vm, so
//	// the graph is built per reconcile rather than shared on the reconciler.
//	func (r *VolumeMigrationReconciler) configFor(
//		vm *v1alpha1.VolumeMigration,
//	) statemachine.Config[phase] {
//		return statemachine.Config[phase]{
//			Initial: pending,
//			States: map[phase]statemachine.StateDef[phase]{
//				// Nothing has been submitted yet, so there is nothing to abort.
//				pending:    {To: []phase{validating, failed}},
//				validating: {To: []phase{running, failed, aborted}, OnEnter: r.onValidating(vm)},
//				// No self-edge: polling a running migration is not a transition,
//				// and re-entering the phase would call ContinueMigration twice.
//				running: {To: []phase{completed, failed, aborted}, OnEnter: r.onRunning(vm)},
//				// Terminal phases, declared with no exits.
//				completed: {OnEnter: r.onFinished(vm)},
//				failed:    {OnEnter: r.onFinished(vm)},
//				aborted:   {OnEnter: r.onFinished(vm)},
//			},
//		}
//	}
//
// A hook performs the side effects of entering its phase and says how long that
// phase may last. Validation runs `nvme connect` against the target node in a
// Job, which can hang if the target is unreachable, so the phase is bounded:
//
//	func (r *VolumeMigrationReconciler) onValidating(vm *v1alpha1.VolumeMigration) statemachine.TransitionFunc[phase] {
//		return func(ctx context.Context, from, to phase) (time.Duration, error) {
//			conns, err := r.storage.CreateMigration(ctx, vm.Status.VolumeUUID, vm.Spec.TargetNodeUUID)
//			if err != nil {
//				return 0, fmt.Errorf("CreateMigration: %w", err)
//			}
//			vm.Status.Connections = conns
//			if err := r.startValidationJob(ctx, vm); err != nil {
//				return 0, err
//			}
//			return 10 * time.Minute, nil
//		}
//	}
//
// The copy itself takes as long as there is data to copy, which is why a hook
// returns its bound rather than declaring it statically in [StateDef]:
//
//	func (r *VolumeMigrationReconciler) onRunning(vm *v1alpha1.VolumeMigration) statemachine.TransitionFunc[phase] {
//		return func(ctx context.Context, from, to phase) (time.Duration, error) {
//			if err := r.storage.ContinueMigration(ctx, vm.Status.MigrationUUID); err != nil {
//				return 0, fmt.Errorf("ContinueMigration: %w", err)
//			}
//			vm.Status.ValidationJobName = "" // the paths checked out; the Job is done
//			return 30*time.Minute + time.Duration(vm.Status.SnapsTotal)*time.Minute, nil
//		}
//	}
//
// A terminal phase needs no bound, so its hook returns a zero duration:
//
//	func (r *VolumeMigrationReconciler) onFinished(vm *v1alpha1.VolumeMigration) statemachine.TransitionFunc[phase] {
//		return func(ctx context.Context, from, to phase) (time.Duration, error) {
//			vm.Status.CompletedAt = &metav1.Time{Time: time.Now()}
//			return 0, r.cleanupValidationJob(ctx, vm)
//		}
//	}
//
// # Kubernetes controllers
//
// A controller's machine does not live between reconciles: the process holds no
// memory of the last pass, and may not even be the same process. The phase is
// therefore kept in the resource and restored on each pass with
// [Machine.Snapshot] and [NewFromSnapshot], which carry both the state and its
// deadline. Restoring runs no entry hooks, on the grounds that the transition
// into that phase already happened and CreateMigration should not be called
// twice.
//
// This assumes one field beyond the existing status.phase: a phaseDeadline
// timestamp for the bound to survive in.
//
//	func (r *VolumeMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
//		var vm v1alpha1.VolumeMigration
//		if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
//			return ctrl.Result{}, client.IgnoreNotFound(err)
//		}
//
//		// An empty phase is a migration nobody has reconciled yet, and
//		// NewFromSnapshot reads it as Config.Initial without being asked.
//		snap := statemachine.Snapshot[phase]{State: vm.Status.Phase}
//		if vm.Status.PhaseDeadline != nil {
//			snap.Deadline = vm.Status.PhaseDeadline.Time
//		}
//		sm, err := statemachine.NewFromSnapshot(ctx, r.configFor(&vm), snap)
//		if err != nil {
//			// An unrecognised phase: a downgrade, or a hand-edited resource.
//			return ctrl.Result{}, fmt.Errorf("restoring phase %q: %w", vm.Status.Phase, err)
//		}
//		defer sm.Close()
//
//		// Completed, Failed and Aborted have no outgoing edges.
//		if sm.IsTerminal() {
//			return ctrl.Result{}, nil
//		}
//
//		// The phase ran out of time — possibly while this controller was down,
//		// since the deadline came back from the resource rather than a live timer.
//		if sm.TimeoutReached() {
//			return ctrl.Result{}, r.failMigration(ctx, &vm, sm,
//				fmt.Errorf("phase %q exceeded its deadline", sm.CurrentState()))
//		}
//
//		if err := r.advance(ctx, &vm, sm); err != nil {
//			return ctrl.Result{}, err
//		}
//
//		snap := sm.Snapshot()
//		vm.Status.Phase = snap.State
//		vm.Status.PhaseDeadline = nil
//		if !snap.Deadline.IsZero() {
//			vm.Status.PhaseDeadline = &metav1.Time{Time: snap.Deadline}
//		}
//		if err := r.Status().Update(ctx, &vm); err != nil {
//			return ctrl.Result{}, err
//		}
//
//		// Poll the backend regularly, but never sleep past the phase deadline.
//		if d, ok := sm.RequeueAfter(); ok {
//			return ctrl.Result{RequeueAfter: min(d, pollInterval)}, nil
//		}
//		return ctrl.Result{RequeueAfter: pollInterval}, nil
//	}
//
// Not every reconcile is a transition. Most passes of a Running migration just
// poll the storage API and update the snapshot counters. The machine only moves
// when the backend reports something new:
//
//	func (r *VolumeMigrationReconciler) advance(ctx context.Context, vm *v1alpha1.VolumeMigration, sm *statemachine.Machine[phase]) error {
//		if vm.Spec.Abort {
//			return r.abort(ctx, vm, sm)
//		}
//		switch sm.CurrentState() {
//		case pending:
//			return sm.TransitionTo(ctx, validating)
//		case validating:
//			if r.validationJobSucceeded(ctx, vm) {
//				return sm.TransitionTo(ctx, running)
//			}
//			return nil // still connecting; the deadline is what bounds the wait
//		case running:
//			status, err := r.storage.MigrationStatus(ctx, vm.Status.MigrationUUID)
//			if err != nil {
//				return err
//			}
//			vm.Status.SnapsMigrated = status.SnapsMigrated
//			if status.Done {
//				return sm.TransitionTo(ctx, completed)
//			}
//			return nil
//		}
//		return nil
//	}
//
// Because the graph declares which phases may be aborted, an abort arriving
// before anything was submitted is rejected by the machine rather than by a
// hand-written switch. That is a distinct outcome from a backend failure, and
// worth telling apart:
//
//	func (r *VolumeMigrationReconciler) abort(ctx context.Context, vm *v1alpha1.VolumeMigration, sm *statemachine.Machine[phase]) error {
//		err := sm.TransitionTo(ctx, aborted)
//		illegal, rejected := errors.AsType[*statemachine.IllegalTransitionError[phase]](err)
//		switch {
//		case err == nil:
//			return nil
//		case rejected:
//			// Nothing reached the storage API from phase illegal.From, so there is
//			// no backend migration to cancel; fail it outright instead.
//			return r.failMigration(ctx, vm, sm,
//				fmt.Errorf("aborted while still %q", illegal.From))
//		default:
//			return err // CancelMigration itself failed; retry on the next pass
//		}
//	}
//
// Two things are worth knowing about this arrangement. A restored deadline is an
// absolute instant rather than a live timer, so it is measured against the wall
// clock and is exposed to skew in a way an in-process deadline is not. And
// persisting the phase without the deadline yields one that can never time out,
// which is why [Machine.Snapshot] returns both together.
//
// # One graph per action
//
// A VolumeMigration does one thing, so one graph describes it. An Ops resource
// does whichever thing its spec.action asked for, and the steps of one action are
// not the steps of another. [MultiConfig] declares them together, keyed by
// [Action], and hands back an ordinary machine for the one that was asked for.
//
// A StorageNodeOps is the case that motivates it. Six actions share one kind, one
// status.subPhase field, and one enum whose values are the union of two disjoint
// workflows:
//
//	remove:  Validating ── Suspending ── Migrating ── Verifying ── Removing
//	migrate: Preparing  ── Restarting ── Promoting
//	shutdown, restart, suspend, resume: no sub-phases at all
//
// Written as one graph, that union is unenforceable, since nothing stops a
// remove op from reporting Promoting. Written as a MultiConfig, the graph for the action in
// hand is the only one in play, and Promoting during a remove is an
// [IllegalTransitionError] rather than an accepted status write.
//
// The outer phase (Pending, Running, Succeeded, and Failed) is identical for
// all six, so it stays a plain [Config]. Such a controller runs two machines: one for
// the phase, and one for the sub-phase of the action it is executing.
package statemachine

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"time"
)

// ErrUnknownState reports a state never declared in [Config.States].
// [New] returns it for an edge into nowhere or a missing initial state, and
// [Machine.TransitionTo] returns it if the machine somehow holds a state the
// configuration does not describe.
var ErrUnknownState = errors.New("statemachine: unknown state")

// ErrClosed reports that the machine has been closed by [Machine.Close], or that
// the context passed to [New] was canceled. It is always joined with the
// underlying context error, so both of these match:
//
//	errors.Is(err, statemachine.ErrClosed)
//	errors.Is(err, context.Canceled)
var ErrClosed = errors.New("statemachine: closed")

// ErrReentrantTransition reports a [Machine.TransitionTo] call made from inside a
// [TransitionFunc], which is rejected because it cannot be honored: the outer
// call has yet to apply its own result and would overwrite the inner one,
// leaving the machine in a state whose entry hook never ran.
//
// A hook that wants to move on immediately should return instead and let its
// caller drive the next transition on the following pass:
//
//	func (r *VolumeMigrationReconciler) onValidating(vm *v1alpha1.VolumeMigration) statemachine.TransitionFunc[phase] {
//		return func(ctx context.Context, from, to phase) (time.Duration, error) {
//			if r.pathsAlreadyConnected(ctx, vm) {
//				// Let the next reconcile move to Running; do not do it from here.
//				return time.Second, nil
//			}
//			return 10 * time.Minute, r.startValidationJob(ctx, vm)
//		}
//	}
var ErrReentrantTransition = errors.New("statemachine: transition already in progress")

// IllegalTransitionError reports a transition the state graph does not allow. It
// is returned by [Machine.TransitionTo] and carries both endpoints, so callers
// can react to the specific edge rather than to failure in general:
//
//	if illegal, ok := errors.AsType[*statemachine.IllegalTransitionError[phase]](err); ok && illegal.To == aborted {
//		// an abort arrived for a migration that was never submitted
//	}
//
// A rejected transition is not a failure of the state it was rejected from: the
// machine is left untouched, including its deadline.
type IllegalTransitionError[S comparable] struct {
	// From is the state the machine was in, and remains in.
	From S
	// To is the state that was requested.
	To S
}

// Error implements the error interface.
func (e *IllegalTransitionError[S]) Error() string {
	return fmt.Sprintf("statemachine: illegal transition %v -> %v", e.From, e.To)
}

// TransitionFunc is the entry hook of a state, invoked by [Machine.TransitionTo]
// once the edge has been validated but before the machine's current state
// changes. It receives both endpoints, so a single hook can serve several
// inbound edges.
//
// The returned duration bounds how long the machine may remain in state to. Zero
// or less means the state has no deadline. The clock starts when the hook
// returns, not when the transition was requested, so a hook that blocks for
// twenty seconds and then asks for a minute yields a state that expires eighty
// seconds after the caller asked for it.
//
// Returning an error aborts the transition: the machine stays in from and keeps
// its existing deadline, and [Machine.TransitionTo] wraps the error. A failing
// hook must therefore not leave its subject half-moved.
//
// A hook must not call [Machine.TransitionTo] on the machine that invoked it.
// Such a call fails with [ErrReentrantTransition] rather than corrupting the
// machine. It may, of course, drive some other machine.
//
// ctx is the context passed to [Machine.TransitionTo] and bounds only this call.
// It is deliberately not the new state's own context, which does not exist
// yet, since this hook's return value defines it. Use [Machine.Context] for work
// that should outlive the transition and be bounded by the state.
type TransitionFunc[S comparable] func(ctx context.Context, from, to S) (time.Duration, error)

// StateDef declares one state's outgoing edges and its entry hook.
type StateDef[S comparable] struct {
	// To lists the states this state may transition to. Every entry must be a
	// key of [Config.States] or [New] fails. Listing the state itself permits a
	// self-transition, which re-runs OnEnter and re-arms the deadline.
	//
	// An empty To declares a terminal state: legal to be in, illegal to leave.
	// That is distinct from a state omitted from Config.States, which is an
	// error.
	To []S

	// OnEnter runs when the machine enters this state. It may be nil, in which
	// case entering always succeeds and leaves the state without a deadline.
	OnEnter TransitionFunc[S]
}

// Config declares a state machine. It is copied by [New], so the caller may
// reuse or mutate it afterward.
type Config[S comparable] struct {
	// Initial is the state the machine starts in, and the state [Machine.Reset]
	// returns to. It must be a key of States. Its OnEnter hook does not run at
	// construction time, because a machine is born already in its initial state.
	Initial S

	// States is the state graph, keyed by state. Because it is a map, a
	// duplicated state is a compile-time error rather than a silent overwrite.
	States map[S]StateDef[S]
}

// validate reports whether the graph is closed: the initial state is declared,
// and no edge points at a state that is not. It is separate from [New] because
// [MultiConfig] validates graphs it is not about to build a machine from.
func (config Config[S]) validate() error {
	if _, ok := config.States[config.Initial]; !ok {
		return fmt.Errorf("%w: initial state %v", ErrUnknownState, config.Initial)
	}
	for state, def := range config.States {
		for _, to := range def.To {
			if _, ok := config.States[to]; !ok {
				return fmt.Errorf("%w: %v -> %v", ErrUnknownState, state, to)
			}
		}
	}
	return nil
}

// Machine is a deterministic finite state machine over the state type S, which
// is typically a named string or integer type so that each machine gets its own
// compile-checked set of states. A resource's existing phase type serves
// directly, with no parallel enum to keep in step:
//
//	statemachine.Machine[v1alpha1.VolumeMigrationPhase]
//
// It must be created by [New] or [Must], since the zero Machine is not usable,
// and released with [Machine.Close]. It is not safe for concurrent use. See the
// package documentation.
type Machine[S comparable] struct {
	base       context.Context
	baseCancel context.CancelFunc

	states  map[S]StateDef[S]
	initial S
	current S

	// entering is the state whose hook is currently running, if any. It guards
	// against a hook transitioning the machine underneath its own caller. See
	// [ErrReentrantTransition].
	entering *S

	stateCtx    context.Context
	stateCancel context.CancelFunc
}

// New validates cfg and returns a machine sitting in its initial state with no
// deadline. The machine must be closed to release its contexts.
//
// ctx bounds the machine: canceling it cancels the current state's context and
// makes [Machine.TransitionTo] fail with [ErrClosed]. Pass a context tied to the
// lifetime of whatever the machine models, never one scoped to a single request.
//
// New reports [ErrUnknownState] if [Config.Initial] is absent from
// [Config.States], or if any state declares an edge to a state that is not declared.
func New[S comparable](ctx context.Context, config Config[S]) (*Machine[S], error) {
	if err := config.validate(); err != nil {
		return nil, err
	}

	states := maps.Clone(config.States)
	for state, def := range states {
		def.To = slices.Clone(def.To)
		states[state] = def
	}

	base, baseCancel := context.WithCancel(ctx)
	sm := &Machine[S]{
		base:       base,
		baseCancel: baseCancel,
		states:     states,
		initial:    config.Initial,
		current:    config.Initial,
	}
	sm.arm(0)
	return sm, nil
}

// Must is [New] for configurations that are known good at compile time, such as
// a literal in a constructor. It panics instead of returning an error because a
// malformed state graph is a bug in the program rather than a runtime condition:
//
//	sm := statemachine.Must(ctx, statemachine.Config[phase]{...})
func Must[S comparable](ctx context.Context, cfg Config[S]) *Machine[S] {
	sm, err := New(ctx, cfg)
	if err != nil {
		panic(err)
	}
	return sm
}

// NewFromSnapshot is [New] followed by [Machine.Restore]: it returns a machine
// already at a persisted position, for the common case of a controller rebuilding
// one from a resource's status. Restoring runs no entry hook, on the grounds that
// the transition into that state already happened in an earlier process.
//
// An empty snapshot yields a machine in [Config.Initial] rather than an error,
// which is the point of this constructor. Persisted phases start out unset,
// since status.phase is empty until the first pass writes one, so every caller
// of [Machine.Restore] otherwise has to guard it:
//
//	// What this replaces.
//	sm := statemachine.Must(ctx, config)
//	if vm.Status.Phase != "" {
//		if err := sm.Restore(snap); err != nil {
//			return ctrl.Result{}, err
//		}
//	}
//
// Precisely: a [Snapshot.State] that is both the zero value of S and not a
// declared state means "fresh," and the snapshot's deadline is dropped with it,
// because a deadline without a state is incoherent and the state is what decides.
// A zero state that *is* declared, as the first constant of an int-backed phase
// type usually is, restores as written, deadline included. A non-zero undeclared
// state stays an error, since that is a downgrade or a hand-edited resource and
// silently starting the workflow over would be the wrong answer.
//
// The returned error is [ErrUnknownState], either for a graph that is not closed
// or for a state the graph does not declare. No machine is returned on either.
func NewFromSnapshot[S comparable](
	ctx context.Context,
	config Config[S],
	snap Snapshot[S],
) (*Machine[S], error) {
	sm, err := New(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := sm.restoreOrStart(snap); err != nil {
		// Close what New just built: an abandoned machine's context would stay
		// attached to ctx until ctx itself is canceled.
		sm.Close()
		return nil, err
	}
	return sm, nil
}

// `arm` replaces the current state's context with one expiring after timeout,
// canceling the previous one. A timeout of zero or less produces a context with
// no deadline, so stateCtx is never nil and [Machine.Deadline] reports the
// absence of a deadline for free.
//
// This is the live path, and it uses a relative timeout so the resulting
// deadline is measured on the monotonic clock and unaffected by wall-clock jumps.
// Restoring a persisted deadline necessarily goes through armAt instead.
func (sm *Machine[S]) arm(timeout time.Duration) {
	if sm.stateCancel != nil {
		sm.stateCancel()
	}
	if timeout > 0 {
		sm.stateCtx, sm.stateCancel = context.WithTimeout(sm.base, timeout)
	} else {
		sm.stateCtx, sm.stateCancel = context.WithCancel(sm.base)
	}
}

// `armAt` is `arm` for an absolute deadline, as read back from persisted state.
// A zero deadline means none. A deadline already in the past yields an
// already-expired context, which is the point: an operator that was down while a
// state timed out must see that timeout on its next pass.
func (sm *Machine[S]) armAt(deadline time.Time) {
	if sm.stateCancel != nil {
		sm.stateCancel()
	}
	if deadline.IsZero() {
		sm.stateCtx, sm.stateCancel = context.WithCancel(sm.base)
	} else {
		sm.stateCtx, sm.stateCancel = context.WithDeadline(sm.base, deadline)
	}
}

// CurrentState returns the state the machine is in.
func (sm *Machine[S]) CurrentState() S {
	return sm.current
}

// Context returns the current state's context. It is canceled when the machine
// leaves the state, when the state's deadline expires, or when the machine is
// closed, whichever comes first.
//
// Carry it for work that is only meaningful while the machine remains in this
// state, and that work unwinds on its own:
//
//	// Abandoned if the migration is aborted or the phase runs out of time.
//	status, err := r.storage.MigrationStatus(sm.Context(), vm.Status.MigrationUUID)
//
// The returned context is replaced on every transition, so fetch it per use
// rather than caching it. [Machine.ClearTimeout] replaces it too: dropping a
// deadline cancels the work that deadline was bounding.
func (sm *Machine[S]) Context() context.Context {
	return sm.stateCtx
}

// Done returns the Done channel of the current state's context, closed when that
// state ends for any reason. Select on it to wake exactly when a deadline
// expires, instead of discovering it up to one tick late:
//
//	select {
//	case <-ticker.C:
//	case <-sm.Done():
//	}
//
// As with [Machine.Context], the channel is replaced on every transition.
func (sm *Machine[S]) Done() <-chan struct{} {
	return sm.stateCtx.Done()
}

// Deadline returns the time at which the current state expires, and whether it
// has one at all. It mirrors [context.Context.Deadline].
func (sm *Machine[S]) Deadline() (time.Time, bool) {
	return sm.stateCtx.Deadline()
}

// TimeoutReached reports whether the current state's deadline has passed. It is
// false for a state without a deadline, and false for a machine that was closed
// or whose context was canceled. Neither is a timeout, and callers there
// generally want to unwind rather than retry.
//
// It keeps reporting true until the machine transitions away or the deadline is
// dropped with [Machine.ClearTimeout], so a caller that handles a timeout
// without transitioning immediately must clear it.
func (sm *Machine[S]) TimeoutReached() bool {
	return sm.base.Err() == nil && errors.Is(sm.stateCtx.Err(), context.DeadlineExceeded)
}

// ClearTimeout drops the current state's deadline without changing state,
// acknowledging a timeout the caller has handled some other way:
//
//	if sm.TimeoutReached() {
//		sm.ClearTimeout()             // do not report it again on the next pass
//		r.extendValidationDeadline(vm) // the operator granted it more time
//	}
//
// It cancels the context returned by [Machine.Context], on the grounds that work
// bounded by a deadline which has just fired is void.
func (sm *Machine[S]) ClearTimeout() {
	sm.arm(0)
}

// TransitionTo moves the machine to state to.
//
// The edge is validated first, then the target's [StateDef.OnEnter] hook runs,
// and only if that succeeds does the current state change and the deadline
// re-arm from the duration the hook returned. Nothing is mutated on any failure
// path.
//
// ctx bounds the hook, not the resulting state. See [TransitionFunc]. The
// returned error is one of:
//
//   - [ErrClosed], joined with the machine context's error if the machine is
//     closed or its context was canceled.
//   - [ErrReentrantTransition] if called from inside a [TransitionFunc] of this
//     same machine.
//   - [ErrUnknownState] if the machine's current state is not declared, which
//     can only happen if the configuration was mutated behind its back.
//   - [IllegalTransitionError] if the graph does not allow this edge. The
//     machine is untouched, including its deadline.
//   - The hook's own error, wrapped with both state names.
func (sm *Machine[S]) TransitionTo(ctx context.Context, to S) error {
	if err := sm.base.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	if sm.entering != nil {
		return fmt.Errorf("%w: entering %v, refused %v", ErrReentrantTransition, *sm.entering, to)
	}

	from := sm.current
	def, ok := sm.states[from]
	if !ok {
		return fmt.Errorf("%w: %v", ErrUnknownState, from)
	}
	if !slices.Contains(def.To, to) {
		return &IllegalTransitionError[S]{From: from, To: to}
	}

	timeout, err := sm.enter(ctx, from, to)
	if err != nil {
		return fmt.Errorf("statemachine: entering %v from %v: %w", to, from, err)
	}

	sm.current = to
	sm.arm(timeout)
	return nil
}

// enter runs to's entry hook, if it has one, with the reentrancy guard raised.
// The guard is lowered on the way out even if the hook panics, so a caller that
// recovers from a panicking hook is left with a usable machine rather than one
// that refuses every subsequent transition.
func (sm *Machine[S]) enter(ctx context.Context, from, to S) (time.Duration, error) {
	onEnter := sm.states[to].OnEnter
	if onEnter == nil {
		return 0, nil
	}
	sm.entering = &to
	defer func() { sm.entering = nil }()
	return onEnter(ctx, from, to)
}

// Reset returns the machine to its initial state and clears any deadline,
// without validating an edge or running any hook. It is the escape hatch for a
// caller that has torn its subject down out of band and needs the machine to
// agree:
//
//	r.cancelBackendMigration(ctx, vm)
//	sm.Reset()
//
// Because it runs no hooks, whatever [Config.Initial]'s OnEnter would have done
// is the caller's responsibility. Do not call it from inside a [TransitionFunc],
// because the in-flight transition would overwrite it on the way out.
func (sm *Machine[S]) Reset() {
	sm.current = sm.initial
	sm.arm(0)
}

// Snapshot is the durable state of a machine: everything needed to reconstruct
// it in a later process. It exists for controllers that keep their machine's
// position in a custom resource rather than in memory, and is deliberately
// free of any Kubernetes dependency.
//
// The JSON field names match the Go ones so the struct can be embedded straight
// into a status stanza, but a CRD generally wants its own fields so the deadline
// can be a [k8s.io/apimachinery/pkg/apis/meta/v1.Time]:
//
//	type VolumeMigrationStatus struct {
//		Phase         VolumeMigrationPhase `json:"phase,omitempty"`
//		PhaseDeadline *metav1.Time         `json:"phaseDeadline,omitempty"`
//	}
type Snapshot[S comparable] struct {
	// State is the state the machine was in.
	State S `json:"state"`

	// Deadline is when that state expires, or the zero time if it has none.
	// It is an absolute wall-clock instant because it has to survive a process
	// restart. See [Machine.Restore] for what that costs.
	Deadline time.Time `json:"deadline,omitzero"`
}

// Snapshot returns the machine's durable state, to be persisted and later handed
// to [Machine.Restore]. Take it after reconciling, and write both fields: a
// snapshot that drops the deadline restores as a state that never times out.
func (sm *Machine[S]) Snapshot() Snapshot[S] {
	var snap Snapshot[S]
	snap.State = sm.current
	if deadline, ok := sm.stateCtx.Deadline(); ok {
		snap.Deadline = deadline
	}
	return snap
}

// Restore places the machine in a previously persisted state, replacing its
// current state and deadline. Like [Machine.Reset] it validates no edge and runs
// no hook: the transition into that state already happened, in an earlier
// process, and its side effects are not to be repeated.
//
// A zero [Snapshot.State] restores nothing and reports [ErrUnknownState] unless
// the zero value is itself a declared state. A controller restoring a resource
// that may never have been reconciled therefore has to guard the call, which is
// what [NewFromSnapshot] exists to absorb. Prefer it when the machine is being
// built in the same breath:
//
//	if vm.Status.Phase != "" {
//		if err := sm.Restore(snap); err != nil {
//			// An unrecognised phase: a downgrade, or a hand-edited resource.
//			return ctrl.Result{}, err
//		}
//	}
//
// The restored deadline is absolute, so unlike a live one it is measured against
// the wall clock and is vulnerable to clock skew between the process that wrote
// it and the one reading it. A deadline already in the past restores as an
// expired one, so [Machine.TimeoutReached] fires on the first pass after a
// controller was down long enough to miss it.
//
// Restore reports [ErrUnknownState] if the state is not declared in
// [Config.States], and [ErrReentrantTransition] if called from inside a
// [TransitionFunc]. It changes nothing on either.
func (sm *Machine[S]) Restore(snap Snapshot[S]) error {
	if sm.entering != nil {
		return fmt.Errorf("%w: entering %v, refused restore of %v", ErrReentrantTransition, *sm.entering, snap.State)
	}
	if _, ok := sm.states[snap.State]; !ok {
		return fmt.Errorf("%w: %v", ErrUnknownState, snap.State)
	}
	sm.current = snap.State
	sm.armAt(snap.Deadline)
	return nil
}

// restoreOrStart is [Machine.Restore], except that the snapshot of a resource
// nothing has reconciled yet leaves the machine where it already is. It backs
// [NewFromSnapshot], which is where the rule and its reasoning are written down.
func (sm *Machine[S]) restoreOrStart(snap Snapshot[S]) error {
	var zero S
	if _, declared := sm.states[snap.State]; !declared && snap.State == zero {
		return nil
	}
	return sm.Restore(snap)
}

// RequeueAfter reports how long remains before the current state expires, and
// whether the caller should wait at all. It answers the question a Kubernetes
// controller actually has:
//
//	if d, ok := sm.RequeueAfter(); ok {
//		return ctrl.Result{RequeueAfter: d}, nil
//	}
//	return ctrl.Result{}, nil
//
// The second result is false when the state has no deadline, and also when that
// deadline has already passed. An expired state needs handling now, not another
// requeue, and controller-runtime would treat a non-positive RequeueAfter as no
// requeue at all. Check [Machine.TimeoutReached] before requeueing.
func (sm *Machine[S]) RequeueAfter() (time.Duration, bool) {
	deadline, ok := sm.stateCtx.Deadline()
	if !ok {
		return 0, false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}
	return remaining, true
}

// Close cancels the current state's context and the machine's own, after which
// [Machine.TransitionTo] fails with [ErrClosed]. It is idempotent and safe to
// defer:
//
//	sm := statemachine.Must(ctx, cfg)
//	defer sm.Close()
//
// It returns nothing, deliberately. Closing only cancels contexts, so there is
// no operation that could fail and nothing a caller could do about it. An error
// return that is always nil would oblige every call site to write
// `defer func() { _ = sm.Close() }()` to satisfy errcheck. This follows
// [context.CancelFunc] and [time.Ticker.Stop] rather than [io.Closer], which is
// for closers that flush.
func (sm *Machine[S]) Close() {
	sm.stateCancel()
	sm.baseCancel()
}

// CanTransitionTo reports whether the graph allows moving to a state right now.
// It says nothing about whether that state's entry hook would succeed.
func (sm *Machine[S]) CanTransitionTo(to S) bool {
	return slices.Contains(sm.states[sm.current].To, to)
}

// IsTerminal reports whether the current state has no outgoing edges, so the
// machine can never move again. For a controller it answers the question worth
// asking first, before any of the work:
//
//	if sm.IsTerminal() {
//		return ctrl.Result{}, nil // Completed, Failed or Aborted: nothing to do
//	}
func (sm *Machine[S]) IsTerminal() bool {
	return len(sm.states[sm.current].To) == 0
}

// AllowedTransitions iterates the states reachable from the current one, in
// declaration order. An empty sequence means the machine is in a terminal state.
// Useful for diagnostics:
//
//	slog.Debug("stuck", "state", sm.CurrentState(),
//		"allowed", slices.Collect(sm.AllowedTransitions()))
func (sm *Machine[S]) AllowedTransitions() iter.Seq[S] {
	return slices.Values(sm.states[sm.current].To)
}

// States iterates the whole declared graph, for rendering it or asserting on it
// in tests. Iteration order is unspecified, as with any map. The yielded
// [StateDef] values share their To slices with the machine, so do not mutate
// them.
func (sm *Machine[S]) States() iter.Seq2[S, StateDef[S]] {
	return maps.All(sm.states)
}
