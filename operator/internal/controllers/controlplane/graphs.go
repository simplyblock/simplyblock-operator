// The state graphs of the installation and of every ControlPlaneOps action,
// both declared as data.
//
// The entity's is a Config rather than a MultiConfig, because a ControlPlane has
// no spec.action to key one on: there is one installation path. The operations'
// is a MultiConfig, and one status.step field serves all three of them, so
// nothing in the API type prevents a Backup from reporting Verifying. The
// per-action graph makes that an IllegalTransitionError at the point of the
// write rather than an accepted status.
//
// design-controlplane.md §4.2 and §6 are the specification.

package controlplane

import (
	"context"
	"time"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// installStep is the entity's step type, aliased so the graph literal reads as
// the graph rather than as a wall of package qualifiers.
type installStep = simplyblockv1alpha2.ControlPlaneStep

const (
	stepApplyingFoundationDB = simplyblockv1alpha2.ControlPlaneStepApplyingFoundationDB
	stepAwaitingFoundationDB = simplyblockv1alpha2.ControlPlaneStepAwaitingFoundationDB
	stepApplyingDatastore    = simplyblockv1alpha2.ControlPlaneStepApplyingDatastore
	stepApplyingAPI          = simplyblockv1alpha2.ControlPlaneStepApplyingAPI
	stepAwaitingAPI          = simplyblockv1alpha2.ControlPlaneStepAwaitingAPI
)

// opsStep is the operations' step type, aliased for the same reason.
type opsStep = simplyblockv1alpha2.ControlPlaneOpsStep

const (
	stepDraining   = simplyblockv1alpha2.ControlPlaneOpsStepDraining
	stepRestarting = simplyblockv1alpha2.ControlPlaneOpsStepRestarting
	stepAwaiting   = simplyblockv1alpha2.ControlPlaneOpsStepAwaiting
	stepPreflight  = simplyblockv1alpha2.ControlPlaneOpsStepPreflight
	stepApplying   = simplyblockv1alpha2.ControlPlaneOpsStepApplying
	stepVerifying  = simplyblockv1alpha2.ControlPlaneOpsStepVerifying
	stepRequesting = simplyblockv1alpha2.ControlPlaneOpsStepRequesting
)

// How long each installation step may take before it is reported as stuck.
//
// The applies are minutes because an apply is a handful of writes against the
// API server, and a step still applying after that is one whose writes are being
// rejected rather than one that is slow. The two waits are the numbers that
// matter.
const (
	applyingFoundationDBDeadline = 10 * time.Minute

	// awaitingFoundationDBDeadline is the step that can take the longest. Three
	// coordinators on slow storage is minutes, an image pull on a cold node is
	// more, and a FoundationDB that never reaches quorum is indistinguishable
	// from one still starting without a bound.
	awaitingFoundationDBDeadline = 45 * time.Minute

	applyingDatastoreDeadline = 10 * time.Minute
	applyingAPIDeadline       = 10 * time.Minute

	// awaitingAPIDeadline covers the management API starting and connecting to
	// the database. It is shorter than FoundationDB's because everything it
	// waits on is already running by the time this step is entered.
	awaitingAPIDeadline = 20 * time.Minute
)

// How long each operation step may take.
const (
	// drainingDeadline holds while other operations in the namespace finish. It
	// is generous because what it waits on is a node add or a volume migration,
	// both of which are legitimately long, and expiring is how an administrator
	// learns the wait is no longer normal.
	drainingDeadline = 2 * time.Hour

	restartingDeadline = 5 * time.Minute

	// awaitingOpsDeadline covers pods coming back, or a backup reporting a
	// snapshot.
	awaitingOpsDeadline = 30 * time.Minute

	preflightDeadline = 10 * time.Minute
	applyingDeadline  = 5 * time.Minute

	// verifyingDeadline covers the rollout finishing and the new version being
	// reported. It is the upgrade's own rolling update plus the probe interval.
	verifyingDeadline = 30 * time.Minute

	requestingDeadline = 5 * time.Minute
)

// deadline is the entry hook every state here carries: it sets the step's budget
// and performs nothing. The side effect of a step is performed on the pass that
// follows, against the step the entry's patch persisted, which is where the
// write-ahead record is needed and what it records.
func deadline[S comparable](d time.Duration) statemachine.TransitionFunc[S] {
	return func(context.Context, S, S) (time.Duration, error) { return d, nil }
}

// installGraph is the entity's own machine (§4.2). It is a line: every step has
// exactly one successor, and AwaitingAPI is terminal because reaching it is what
// makes the control plane Available.
func installGraph() statemachine.Config[installStep] {
	return statemachine.Config[installStep]{
		Initial: stepApplyingFoundationDB,
		States: map[installStep]statemachine.StateDef[installStep]{
			stepApplyingFoundationDB: {
				To:      []installStep{stepAwaitingFoundationDB},
				OnEnter: deadline[installStep](applyingFoundationDBDeadline),
			},
			stepAwaitingFoundationDB: {
				To:      []installStep{stepApplyingDatastore},
				OnEnter: deadline[installStep](awaitingFoundationDBDeadline),
			},
			stepApplyingDatastore: {
				To:      []installStep{stepApplyingAPI},
				OnEnter: deadline[installStep](applyingDatastoreDeadline),
			},
			stepApplyingAPI: {
				To:      []installStep{stepAwaitingAPI},
				OnEnter: deadline[installStep](applyingAPIDeadline),
			},
			stepAwaitingAPI: {OnEnter: deadline[installStep](awaitingAPIDeadline)},
		},
	}
}

// installStepBudgets is what each step's deadline was set from, derived from the
// graph's constants rather than restated so that a budget changed in one place
// moves both. It is what sets the first step's deadline, which a machine born
// already in that step would otherwise never get.
var installStepBudgets = map[installStep]time.Duration{
	stepApplyingFoundationDB: applyingFoundationDBDeadline,
	stepAwaitingFoundationDB: awaitingFoundationDBDeadline,
	stepApplyingDatastore:    applyingDatastoreDeadline,
	stepApplyingAPI:          applyingAPIDeadline,
	stepAwaitingAPI:          awaitingAPIDeadline,
}

// opsGraphs declares one state graph per operation action over one step type.
//
// Restart and Upgrade both carry Draining, because both roll the same
// Deployment: an upgrade replaces the management API's image, which recycles its
// pods exactly as a restart does, and an operation interrupted by that has been
// interrupted whichever field caused it. Backup carries none, because asking
// FoundationDB for a snapshot recycles nothing.
//
// Upgrade drains after Preflight rather than before it. An upgrade refused for
// naming the image already running is refused in a moment, and making it first
// wait out a twenty-minute node add would spend the fleet's time to reach an
// error that was available immediately.
func opsGraphs() statemachine.MultiConfig[opsStep] {
	return statemachine.MultiConfig[opsStep]{
		action(simplyblockv1alpha2.ControlPlaneOpsActionRestart): {
			Initial: stepDraining,
			States: map[opsStep]statemachine.StateDef[opsStep]{
				stepDraining: {
					To:      []opsStep{stepRestarting},
					OnEnter: deadline[opsStep](drainingDeadline),
				},
				stepRestarting: {
					To:      []opsStep{stepAwaiting},
					OnEnter: deadline[opsStep](restartingDeadline),
				},
				stepAwaiting: {OnEnter: deadline[opsStep](awaitingOpsDeadline)},
			},
		},

		action(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade): {
			Initial: stepPreflight,
			States: map[opsStep]statemachine.StateDef[opsStep]{
				stepPreflight: {
					To:      []opsStep{stepDraining},
					OnEnter: deadline[opsStep](preflightDeadline),
				},
				stepDraining: {
					To:      []opsStep{stepApplying},
					OnEnter: deadline[opsStep](drainingDeadline),
				},
				stepApplying: {
					To:      []opsStep{stepAwaiting},
					OnEnter: deadline[opsStep](applyingDeadline),
				},
				stepAwaiting: {
					To:      []opsStep{stepVerifying},
					OnEnter: deadline[opsStep](awaitingOpsDeadline),
				},
				stepVerifying: {OnEnter: deadline[opsStep](verifyingDeadline)},
			},
		},

		action(simplyblockv1alpha2.ControlPlaneOpsActionBackup): {
			Initial: stepRequesting,
			States: map[opsStep]statemachine.StateDef[opsStep]{
				stepRequesting: {
					To:      []opsStep{stepAwaiting},
					OnEnter: deadline[opsStep](requestingDeadline),
				},
				stepAwaiting: {OnEnter: deadline[opsStep](awaitingOpsDeadline)},
			},
		},
	}
}

// opsInitialDeadlines are the budgets of the step each action's machine is born
// in. A machine is already in its initial state when it is built, so that
// state's OnEnter never runs and the graph's deadline for it is never set.
// Setting it explicitly is what stops the first step of every operation from
// being the one step that cannot time out.
var opsInitialDeadlines = map[statemachine.Action]time.Duration{
	action(simplyblockv1alpha2.ControlPlaneOpsActionRestart): drainingDeadline,
	action(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade): preflightDeadline,
	action(simplyblockv1alpha2.ControlPlaneOpsActionBackup):  requestingDeadline,
}

// abortableSteps are the steps from which an abort stops the operation cleanly.
//
// The line is whether anything has been changed yet. Draining and Preflight have
// performed no side effect at all, and Requesting has not yet created the
// backup. Everything past those has rolled a Deployment or written an image onto
// the entity, and the operation is what drives that rollout to completion.
//
// It is a table beside the graph rather than an edge in it: the phase already
// carries what a terminal Aborted step would say. A test asserts every step here
// is one some graph declares, so the two cannot drift.
var abortableSteps = map[opsStep]bool{
	stepDraining:   true,
	stepPreflight:  true,
	stepRequesting: true,
}

// abortable reports whether an abort asked for while the operation sits on this
// step can be honored.
func abortable(current opsStep) bool { return abortableSteps[current] }

// action converts the API's action enum into the MultiConfig's key. The
// conversion exists because statemachine.Action is a concrete string type rather
// than a second type parameter, and doing it in one place keeps the graph
// literal readable.
func action(a simplyblockv1alpha2.ControlPlaneOpsAction) statemachine.Action {
	return statemachine.Action(a)
}
