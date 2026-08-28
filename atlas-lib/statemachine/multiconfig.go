package statemachine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ErrUnknownAction reports an action that a [MultiConfig] declares no graph for.
//
// Unlike a malformed graph, this is not a programmer error and so is returned
// rather than panicked on: the action comes from a resource's spec, which
// survives a downgrade and a hand-edited object.
//
// It is also what a caller gets for an action that legitimately has no states at
// all: the StorageNodeOps shutdown, restart, suspend, and resume actions have no
// sub-phases to track. That is a normal condition rather than a failure, so ask
// the map instead of reading the error, which [MultiConfig] being a map type is
// partly for:
//
//	if _, ok := subPhases[action]; !ok {
//		return ctrl.Result{}, nil // this action runs in a single step
//	}
var ErrUnknownAction = errors.New("statemachine: unknown action")

// Action names one variant of a [MultiConfig], here the value of an Ops
// resource's spec.action field.
//
// It is a concrete string type rather than a second type parameter, so that
// MultiConfig takes the same single parameter [Config] does. The trade-off is
// deliberate: the states are what this package makes safe, whereas the action is
// an external discriminator that the CRD's own enum marker already validated at
// admission.
type Action string

// MultiConfig declares one state graph per action, over a single state type. It
// is the shape an Ops resource has: one kind, one status field, and a workflow
// that depends on what was asked for.
//
// A StorageNodeOps is the worked case. Its sub-phase enum is the union of two
// disjoint workflows, so nothing but the graph stops a remove op from reporting a
// step that belongs only to a migrate:
//
//	remove:  Validating ── Suspending ── Migrating ── Verifying ── Removing
//	migrate: Preparing  ── Restarting ── Promoting
//
// Declaring both as one MultiConfig is what makes that union stop being a union
// in practice. Promoting during a remove becomes an [IllegalTransitionError]
// instead of an accepted status write:
//
//	// A type alias keeps the map literal readable, and the inner Config[subPhase]
//	// is elided because MultiConfig is a map type.
//	type subPhase = v1alpha1.StorageNodeOpsSubPhase
//
//	func (r *StorageNodeOpsReconciler) subPhasesFor(
//		op *v1alpha1.StorageNodeOps,
//	) statemachine.MultiConfig[subPhase] {
//		return statemachine.MultiConfig[subPhase]{
//			"remove": {
//				Initial: validating,
//				States: map[subPhase]statemachine.StateDef[subPhase]{
//					validating: {To: []subPhase{suspending}, OnEnter: r.onValidating(op)},
//					suspending: {To: []subPhase{migrating}, OnEnter: r.onSuspending(op)},
//					migrating:  {To: []subPhase{verifying}, OnEnter: r.onMigrating(op)},
//					verifying:  {To: []subPhase{removing}, OnEnter: r.onVerifying(op)},
//					removing:   {OnEnter: r.onRemoving(op)},
//				},
//			},
//			"migrate": {
//				Initial: preparing,
//				States: map[subPhase]statemachine.StateDef[subPhase]{
//					preparing:  {To: []subPhase{restarting}, OnEnter: r.onPreparing(op)},
//					restarting: {To: []subPhase{promoting}, OnEnter: r.onRestarting(op)},
//					promoting:  {OnEnter: r.onPromoting(op)},
//				},
//			},
//			// shutdown, restart, suspend, and resume run in a single step and
//			// declare no graph at all; see [ErrUnknownAction].
//		}
//	}
//
// The outer phase (Pending, Running, Succeeded, and Failed) is the same for
// every action, so it stays an ordinary [Config] and an ordinary [Machine]. Two
// machines, not one merged graph: folding the shared phases into each action's
// map would copy that spine once per action, and a fix would land in one of them.
//
// Because it is a map, the declared actions can be inspected directly, and a
// [MultiConfig] built per reconcile can have an action's graph swapped before it
// is used. The graphs are copied when a machine is built, as in [New].
type MultiConfig[S comparable] map[Action]Config[S]

// configFor validates every declared graph, then returns the one for action.
//
// Validating all of them, rather than only the selected one, is most of what
// distinguishes this type from a switch over spec.action. A switch validates the
// branch it takes, so a typo in the migrate graph goes undiscovered until someone
// migrates. Here, constructing a machine for any action proves every action
// sound. The graphs are literals in the binary, so the cost is a map walk and the
// failure surfaces on the first test that builds a machine.
//
// Graphs are checked in action order, so a configuration with two faults reports
// the same one every time. They are checked before the action is looked up, so a
// bug in the program is never masked by bad input.
func (mc MultiConfig[S]) configFor(action Action) (Config[S], error) {
	for _, declared := range slices.Sorted(maps.Keys(mc)) {
		if err := mc[declared].validate(); err != nil {
			return Config[S]{}, fmt.Errorf("action %v: %w", declared, err)
		}
	}
	config, ok := mc[action]
	if !ok {
		return Config[S]{}, fmt.Errorf("%w: %v", ErrUnknownAction, action)
	}
	return config, nil
}

// New returns a machine for one action's graph, sitting in that graph's initial
// state with no deadline. The machine is an ordinary [Machine] and must be closed
// to release its contexts, and ctx bounds it exactly as it does in [New].
//
// There is no Must counterpart, deliberately. Must exists because a malformed
// graph is a bug in the program, whereas an unrecognized action is user input,
// so offering it would invite panicking a controller on a hand-edited
// spec.action. A
// malformed graph still fails here, through the returned error rather than a
// panic, and it fails whichever action was asked for.
//
// The returned error is [ErrUnknownState] for a graph that is not closed, or
// [ErrUnknownAction] for an action this MultiConfig does not declare.
func (mc MultiConfig[S]) New(ctx context.Context, action Action) (*Machine[S], error) {
	config, err := mc.configFor(action)
	if err != nil {
		return nil, err
	}
	return New(ctx, config)
}

// FromSnapshot returns a machine for one action's graph, already restored to a
// persisted position. It is [MultiConfig.New] followed by [Machine.Restore], and
// it is how a controller picks its sub-phase machine back up:
//
//	action := statemachine.Action(op.Spec.Action)
//	snap := statemachine.Snapshot[subPhase]{State: op.Status.SubPhase, Deadline: end}
//	sm, err := r.subPhasesFor(op).FromSnapshot(ctx, action, snap)
//
// The action is passed separately rather than carried in the snapshot, because it
// is not machine state: spec.action is immutable, so it is re-read from the
// resource on every pass and nothing about it belongs in status.
//
// An empty snapshot means a resource nobody has reconciled yet, and yields a
// machine in [Config.Initial]. See [NewFromSnapshot], whose rules this shares.
// Restoring runs no entry hook.
func (mc MultiConfig[S]) FromSnapshot(
	ctx context.Context,
	action Action,
	snap Snapshot[S],
) (*Machine[S], error) {
	config, err := mc.configFor(action)
	if err != nil {
		return nil, err
	}
	return NewFromSnapshot(ctx, config, snap)
}
