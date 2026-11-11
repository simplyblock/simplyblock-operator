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
	stepBuildingIndices      = simplyblockv1alpha2.ControlPlaneStepBuildingIndices
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

	// The HostMaintenance window's steps (design-fdb-host-maintenance.md §6).
	stepHolding             = simplyblockv1alpha2.ControlPlaneOpsStepHolding
	stepEnteringMaintenance = simplyblockv1alpha2.ControlPlaneOpsStepEnteringMaintenance
	stepMovingCoordinators  = simplyblockv1alpha2.ControlPlaneOpsStepMovingCoordinators
	stepReleasing           = simplyblockv1alpha2.ControlPlaneOpsStepReleasing
	stepAwaitingHost        = simplyblockv1alpha2.ControlPlaneOpsStepAwaitingHost
	stepLeavingMaintenance  = simplyblockv1alpha2.ControlPlaneOpsStepLeavingMaintenance
	stepCleanup             = simplyblockv1alpha2.ControlPlaneOpsStepCleanup
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

	// buildingIndicesDeadline covers pulling the control plane's image and
	// walking a database that holds nothing, so what it really bounds is a Job
	// that cannot be scheduled and a backfill that cannot reach the database.
	buildingIndicesDeadline = 15 * time.Minute

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

	// The window's budgets (design-fdb-host-maintenance.md §6). holdingDeadline
	// runs from the last drain any window in the namespace observed rather than
	// from the window's creation, which hostmaintenance.go re-arms, so a pool
	// whose update is progressing never expires a window waiting its turn.
	// awaitingHostDeadline is the storage node's, since both wait for the same
	// reboot, and it is also how long the maintenance zone is set for.
	holdingDeadline             = 6 * time.Hour
	enteringMaintenanceDeadline = 5 * time.Minute
	movingCoordinatorsDeadline  = 10 * time.Minute
	releasingDeadline           = 5 * time.Minute
	awaitingHostDeadline        = 4 * time.Hour
	leavingMaintenanceDeadline  = 5 * time.Minute
	cleanupDeadline             = 5 * time.Minute
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
		Initial: stepApplyingDatastore,
		States: map[installStep]statemachine.StateDef[installStep]{
			stepApplyingDatastore: {
				To:      []installStep{stepApplyingFoundationDB},
				OnEnter: deadline[installStep](applyingDatastoreDeadline),
			},
			stepApplyingFoundationDB: {
				To:      []installStep{stepAwaitingFoundationDB},
				OnEnter: deadline[installStep](applyingFoundationDBDeadline),
			},
			stepAwaitingFoundationDB: {
				To:      []installStep{stepBuildingIndices},
				OnEnter: deadline[installStep](awaitingFoundationDBDeadline),
			},
			stepBuildingIndices: {
				To:      []installStep{stepApplyingAPI},
				OnEnter: deadline[installStep](buildingIndicesDeadline),
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
	stepBuildingIndices:      buildingIndicesDeadline,
	stepApplyingDatastore:    applyingDatastoreDeadline,
	stepApplyingAPI:          applyingAPIDeadline,
	stepAwaitingAPI:          awaitingAPIDeadline,
}

// opsGraphs declares one state graph per operation action over one step type.
//
// Restart and Upgrade both carry Draining, because both roll the same
// Deployment: an upgrade replaces the management API's image, which restarts its
// pods exactly as a restart does, and an operation interrupted by that has been
// interrupted whichever field caused it. Backup carries none, because asking
// FoundationDB for a snapshot restarts nothing.
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
					To:        []opsStep{stepRestarting},
					Abortable: true,
					OnEnter:   deadline[opsStep](drainingDeadline),
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
					To:        []opsStep{stepDraining},
					Abortable: true,
					OnEnter:   deadline[opsStep](preflightDeadline),
				},
				stepDraining: {
					To:        []opsStep{stepApplying},
					Abortable: true,
					OnEnter:   deadline[opsStep](drainingDeadline),
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
					To:        []opsStep{stepAwaiting},
					Abortable: true,
					OnEnter:   deadline[opsStep](requestingDeadline),
				},
				stepAwaiting: {OnEnter: deadline[opsStep](awaitingOpsDeadline)},
			},
		},

		// HostMaintenance holds a worker's FoundationDB pods from the cordon,
		// moves the roles off the worker when its drain arrives, and waits for
		// the processes to come back. Holding is the one abortable step: nothing
		// has been done to the database from there, and from EnteringMaintenance
		// on something has to clear what was set.
		action(simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance): {
			Initial: stepHolding,
			States: map[opsStep]statemachine.StateDef[opsStep]{
				stepHolding: {
					To:        []opsStep{stepEnteringMaintenance},
					Abortable: true,
					OnEnter:   deadline[opsStep](holdingDeadline),
				},
				stepEnteringMaintenance: {
					To:      []opsStep{stepMovingCoordinators},
					OnEnter: deadline[opsStep](enteringMaintenanceDeadline),
				},
				stepMovingCoordinators: {
					To:      []opsStep{stepReleasing},
					OnEnter: deadline[opsStep](movingCoordinatorsDeadline),
				},
				stepReleasing: {
					To:      []opsStep{stepAwaitingHost},
					OnEnter: deadline[opsStep](releasingDeadline),
				},
				stepAwaitingHost: {
					To:      []opsStep{stepLeavingMaintenance},
					OnEnter: deadline[opsStep](awaitingHostDeadline),
				},
				stepLeavingMaintenance: {
					To:      []opsStep{stepCleanup},
					OnEnter: deadline[opsStep](leavingMaintenanceDeadline),
				},
				stepCleanup: {OnEnter: deadline[opsStep](cleanupDeadline)},
			},
		},
	}
}

// opsLockedSteps declares, per action, the steps that hold the control plane's
// lock. An action absent from the map holds it from its first step to its last,
// which is what Restart, Upgrade, and Backup always did. The HostMaintenance
// window holds it only while it touches the database and chooses among hosts
// (design-fdb-host-maintenance.md §7.3): Holding is a budget the window owns,
// and the wait for the processes to return excludes nothing.
var opsLockedSteps = map[statemachine.Action]map[opsStep]bool{
	action(simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance): {
		stepEnteringMaintenance: true,
		stepMovingCoordinators:  true,
		stepReleasing:           true,
	},
}

// holdsLock reports whether an action's step runs under the control plane's
// lock.
func holdsLock(act simplyblockv1alpha2.ControlPlaneOpsAction, step opsStep) bool {
	locked, declared := opsLockedSteps[action(act)]
	if !declared {
		return true
	}
	return locked[step]
}

// locksFromTheStart reports whether an action takes the lock before its first
// step, which is every action whose first step holds it.
func locksFromTheStart(act simplyblockv1alpha2.ControlPlaneOpsAction) bool {
	return holdsLock(act, opsGraphs()[action(act)].Initial)
}

// opsInitialDeadlines are the budgets of the step each action's machine is born
// in. A machine is already in its initial state when it is built, so that
// state's OnEnter never runs and the graph's deadline for it is never set.
// Setting it explicitly is what stops the first step of every operation from
// being the one step that cannot time out.
var opsInitialDeadlines = map[statemachine.Action]time.Duration{
	action(simplyblockv1alpha2.ControlPlaneOpsActionRestart):         drainingDeadline,
	action(simplyblockv1alpha2.ControlPlaneOpsActionUpgrade):         preflightDeadline,
	action(simplyblockv1alpha2.ControlPlaneOpsActionBackup):          requestingDeadline,
	action(simplyblockv1alpha2.ControlPlaneOpsActionHostMaintenance): holdingDeadline,
}

// Which steps an abort stops cleanly is declared on the states above, and the
// line it draws is whether anything has been changed yet. Draining and Preflight
// have performed no side effect at all, and Requesting has not yet created the
// backup. Everything past those has rolled a Deployment or written an image onto
// the entity, and the operation is what drives that rollout to completion.
//
// It is a property of the state rather than an edge to a terminal one: the phase
// already carries what a terminal Aborted step would say.

// UnabortableSteps are the steps no action can be stopped from, sorted.
//
// It is exported for the DELETE guard on this kind, which asks the same question
// this package's unwind asks: a deletion may not express something spec.abort
// could not, so both channels read one graph (design-crd-model.md §3.1). The
// guard has a step out of a status and no machine, which is the whole reason
// this reads the graphs rather than the machine the reconciler holds.
func UnabortableSteps() []opsStep {
	return statemachine.UnabortableMultiStates(opsGraphs())
}

// action converts the API's action enum into the MultiConfig's key. The
// conversion exists because statemachine.Action is a concrete string type rather
// than a second type parameter, and doing it in one place keeps the graph
// literal readable.
func action(a simplyblockv1alpha2.ControlPlaneOpsAction) statemachine.Action {
	return statemachine.Action(a)
}
