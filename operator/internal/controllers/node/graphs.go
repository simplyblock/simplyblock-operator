// The state graph of every StorageNodeOps action, and of the StorageNode's own
// provisioning path, both declared as data.
//
// One status.step field serves all seven operations, so nothing in the API type
// prevents a Remove from reporting Promoting. The per-action graph makes that an
// IllegalTransitionError at the point of the write rather than an accepted status
// (design-crd-model.md §3.1), which is the whole reason the steps are declared
// here instead of switched on in the reconciler.
//
// MultiConfig validates every declared graph whenever a machine is built for any
// of them, so a bad edge in HostMaintenance — the action that only runs during an
// OS upgrade and is the most expensive to exercise — is caught by any test that
// builds a machine at all.
//
// The entity's graph is a Config rather than a MultiConfig, because a StorageNode
// has no spec.action to key one on: there is one provisioning path and adoption
// is a branch within it.
//
// design-storagenode.md §4.2, §6.3, §8.2, §9, and §10 are the specification.

package node

import (
	"context"
	"time"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// step is the operation's step type, aliased so the graph literals below read as
// the graphs rather than as a wall of package qualifiers.
type step = simplyblockv1alpha2.StorageNodeOpsStep

const (
	stepRequesting       = simplyblockv1alpha2.StorageNodeOpsStepRequesting
	stepAwaiting         = simplyblockv1alpha2.StorageNodeOpsStepAwaiting
	stepValidating       = simplyblockv1alpha2.StorageNodeOpsStepValidating
	stepSuspending       = simplyblockv1alpha2.StorageNodeOpsStepSuspending
	stepMigratingVolumes = simplyblockv1alpha2.StorageNodeOpsStepMigratingVolumes
	stepVerifying        = simplyblockv1alpha2.StorageNodeOpsStepVerifying
	stepRemoving         = simplyblockv1alpha2.StorageNodeOpsStepRemoving
	stepPreparing        = simplyblockv1alpha2.StorageNodeOpsStepPreparing
	stepRelocating       = simplyblockv1alpha2.StorageNodeOpsStepRelocating
	stepAwaitingNode     = simplyblockv1alpha2.StorageNodeOpsStepAwaitingNode
	stepPromoting        = simplyblockv1alpha2.StorageNodeOpsStepPromoting
	stepHolding          = simplyblockv1alpha2.StorageNodeOpsStepHolding
	stepShuttingDown     = simplyblockv1alpha2.StorageNodeOpsStepShuttingDown
	stepReleasing        = simplyblockv1alpha2.StorageNodeOpsStepReleasing
	stepAwaitingHost     = simplyblockv1alpha2.StorageNodeOpsStepAwaitingHost
	stepRestarting       = simplyblockv1alpha2.StorageNodeOpsStepRestarting
	stepCleanup          = simplyblockv1alpha2.StorageNodeOpsStepCleanup
)

// nodeStep is the entity's step type, aliased for the same reason.
type nodeStep = simplyblockv1alpha2.StorageNodeStep

const (
	stepCheckingHost   = simplyblockv1alpha2.StorageNodeStepCheckingHost
	stepCheckingConfig = simplyblockv1alpha2.StorageNodeStepCheckingConfig
	stepAwaitingSlot   = simplyblockv1alpha2.StorageNodeStepAwaitingSlot
	stepPosting        = simplyblockv1alpha2.StorageNodeStepPosting
	stepResolving      = simplyblockv1alpha2.StorageNodeStepResolving
	stepAdopting       = simplyblockv1alpha2.StorageNodeStepAdopting

	stepAwaitingWorker = simplyblockv1alpha2.StorageNodeStepAwaitingWorker
)

// How long each operation step may take before it is reported as stuck.
//
// They differ by what the step is waiting on rather than by preference. A request
// is one HTTP call. A node coming back from a restart is a data-plane operation
// across every device it owns. Two are deliberately generous and finite, and §16
// records that neither number is known to be right: Validating holds on a human
// removing a pin, and MigratingVolumes holds on a node's worth of volumes moving,
// which on a hundred large volumes is hours. What a deadline separates there is a
// drain waiting by design from one waiting because of a bug, and without one the
// two look identical.
const (
	requestingDeadline  = 2 * time.Minute
	awaitingDeadline    = 30 * time.Minute
	validatingDeadline  = 24 * time.Hour
	suspendingDeadline  = 15 * time.Minute
	migratingDeadline   = 12 * time.Hour
	verifyingDeadline   = 30 * time.Minute
	removingDeadline    = 30 * time.Minute
	preparingDeadline   = 15 * time.Minute
	relocatingDeadline  = 15 * time.Minute
	nodeRestartDeadline = 45 * time.Minute
	promotingDeadline   = 30 * time.Minute
	holdingDeadline     = 6 * time.Hour
	releasingDeadline   = 15 * time.Minute

	// awaitingHostDeadline is the step nobody controls the length of: an OS
	// upgrade and a reboot take as long as they take, and a firmware update is
	// the case that sets the number. An expiry fails the operation and leaves the
	// node offline, needing a Restart to recover, so this is a detection
	// mechanism rather than a recovery one (§10).
	awaitingHostDeadline = 4 * time.Hour

	cleanupDeadline = 5 * time.Minute
)

// How long each step of the entity's provisioning path may take.
//
// Resolving is the one that matters most: a node add the control plane accepted
// and then failed to complete leaves the object polling for a UUID that never
// arrives, which is the failure mode status.status: timeout names today with no
// bound behind it (§4.2).
const (
	checkingHostDeadline   = 30 * time.Minute
	checkingConfigDeadline = 24 * time.Hour
	awaitingSlotDeadline   = 4 * time.Hour
	postingDeadline        = 10 * time.Minute
	resolvingDeadline      = 45 * time.Minute
	adoptingDeadline       = 10 * time.Minute

	// A worker comes back from a MachineConfig reboot in minutes: the cordon,
	// the drain, the reboot itself and the uncordon took eleven of them on the
	// cluster this was written for. The budget is the one that says a machine is
	// not coming back rather than the one that says it is slow, so it is an hour.
	awaitingWorkerDeadline = time.Hour
)

// deadline is the entry hook every state here carries: it sets the step's budget
// and performs nothing. The side effect of a step is performed on the pass that
// follows, against the step the entry's patch persisted, which is where the
// write-ahead record is needed and what it records.
func deadline[S comparable](d time.Duration) statemachine.TransitionFunc[S] {
	return func(context.Context, S, S) (time.Duration, error) { return d, nil }
}

// graphs declares one state graph per operation action over one step type.
func graphs() statemachine.MultiConfig[step] {
	// requestAndWait is the two-step line the four single-step actions share:
	// post the action, then wait for the completion condition. It is a function
	// rather than a shared value because MultiConfig copies the graph when a
	// machine is built and a shared map would be one graph under four keys.
	requestAndWait := func() statemachine.Config[step] {
		return statemachine.Config[step]{
			Initial: stepRequesting,
			States: map[step]statemachine.StateDef[step]{
				stepRequesting: {
					To:        []step{stepAwaiting},
					Abortable: true,
					OnEnter:   deadline[step](requestingDeadline),
				},
				stepAwaiting: {OnEnter: deadline[step](awaitingDeadline)},
			},
		}
	}

	return statemachine.MultiConfig[step]{
		action(simplyblockv1alpha2.StorageNodeOpsActionShutdown): requestAndWait(),
		action(simplyblockv1alpha2.StorageNodeOpsActionRestart):  requestAndWait(),
		action(simplyblockv1alpha2.StorageNodeOpsActionSuspend):  requestAndWait(),
		action(simplyblockv1alpha2.StorageNodeOpsActionResume):   requestAndWait(),

		// Validation runs before the suspend, and that ordering is the design: a
		// suspended node accepts no new volume placement, so suspending one whose
		// drain cannot complete takes capacity out of the cluster and leaves it
		// out for as long as the blocker goes unnoticed (§8.2).
		action(simplyblockv1alpha2.StorageNodeOpsActionRemove): {
			Initial: stepValidating,
			States: map[step]statemachine.StateDef[step]{
				// Validating performs no side effect at all, which is what makes
				// an abort there an Aborted directly rather than an unwind. The
				// three steps past the suspend are abortable because their
				// unwind exists: the resume the graph already performs on every
				// other terminal outcome from Suspending onward (§8.3).
				// Removing is not, because the node is being taken out of the
				// cluster and there is no resume that puts it back.
				stepValidating: {
					To:        []step{stepSuspending},
					Abortable: true,
					OnEnter:   deadline[step](validatingDeadline),
				},
				stepSuspending: {
					To:        []step{stepMigratingVolumes},
					Abortable: true,
					OnEnter:   deadline[step](suspendingDeadline),
				},
				stepMigratingVolumes: {
					To:        []step{stepVerifying},
					Abortable: true,
					OnEnter:   deadline[step](migratingDeadline),
				},
				stepVerifying: {
					To:        []step{stepRemoving},
					Abortable: true,
					OnEnter:   deadline[step](verifyingDeadline),
				},
				stepRemoving: {OnEnter: deadline[step](removingDeadline)},
			},
		},

		// Relocating and AwaitingNode are two steps because one would race. The
		// restart is asynchronous, so a node still reporting online immediately
		// after the call may be reporting the state from before it: Relocating
		// completes when the node has left online, and AwaitingNode when it is
		// back. Collapsing them means /promote can be issued while the restart's
		// own node writes are in flight, which leaves the relocated devices stuck
		// in `new` (§9).
		action(simplyblockv1alpha2.StorageNodeOpsActionMigrate): {
			Initial: stepPreparing,
			States: map[step]statemachine.StateDef[step]{
				// Preparing has labeled a target host and nothing more.
				// Everything after it is refused: the node is mid-restart with
				// this operation the only thing watching it back, and the
				// promote has re-homed the logical volumes, so there is nothing
				// to unwind and the operation is what finishes the relocation.
				stepPreparing: {
					To:        []step{stepRelocating},
					Abortable: true,
					OnEnter:   deadline[step](preparingDeadline),
				},
				stepRelocating: {
					To:      []step{stepAwaitingNode},
					OnEnter: deadline[step](relocatingDeadline),
				},
				stepAwaitingNode: {
					To:      []step{stepPromoting},
					OnEnter: deadline[step](nodeRestartDeadline),
				},
				stepPromoting: {OnEnter: deadline[step](promotingDeadline)},
			},
		},

		action(simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance): {
			Initial: stepHolding,
			States: map[step]statemachine.StateDef[step]{
				// Holding is the window before the node is taken down, and the
				// last point at which calling the maintenance off costs
				// nothing. From ShuttingDown onward the node is being taken
				// down for a reboot nothing else will bring it back from.
				stepHolding: {
					To:        []step{stepShuttingDown},
					Abortable: true,
					OnEnter:   deadline[step](holdingDeadline),
				},
				stepShuttingDown: {
					To:      []step{stepReleasing},
					OnEnter: deadline[step](suspendingDeadline),
				},
				stepReleasing: {
					To:      []step{stepAwaitingHost},
					OnEnter: deadline[step](releasingDeadline),
				},
				stepAwaitingHost: {
					To:      []step{stepRestarting},
					OnEnter: deadline[step](awaitingHostDeadline),
				},
				stepRestarting: {
					To:      []step{stepCleanup},
					OnEnter: deadline[step](nodeRestartDeadline),
				},
				stepCleanup: {OnEnter: deadline[step](cleanupDeadline)},
			},
		},
	}
}

// provisioningGraph is the entity's own machine (§4.2).
//
// CheckingHost declares three successors because adoption diverts from it: an
// upgrade Secret or a backend node already at the worker's address sends the node
// to Adopting, and everything else continues to the configuration gate. Adopting
// and Resolving are both terminal, because both end with a UUID on the object and
// the node in steady state.
func provisioningGraph() statemachine.Config[nodeStep] {
	return statemachine.Config[nodeStep]{
		Initial: stepCheckingHost,
		States: map[nodeStep]statemachine.StateDef[nodeStep]{
			stepCheckingHost: {
				To:      []nodeStep{stepCheckingConfig, stepAdopting},
				OnEnter: deadline[nodeStep](checkingHostDeadline),
			},
			stepCheckingConfig: {
				To:      []nodeStep{stepAwaitingSlot, stepAdopting},
				OnEnter: deadline[nodeStep](checkingConfigDeadline),
			},
			stepAwaitingSlot: {
				// Adopting is an exit because the backend node a queuing node
				// would have added can appear while it queues, and a node that
				// has one to take over must not ask for a second.
				To:      []nodeStep{stepPosting, stepResolving, stepAdopting},
				OnEnter: deadline[nodeStep](awaitingSlotDeadline),
			},
			stepPosting: {
				To:      []nodeStep{stepResolving, stepAwaitingWorker},
				OnEnter: deadline[nodeStep](postingDeadline),
			},
			stepResolving: {
				// Posting is an exit because an add that left the control
				// plane's task window without producing a node is one to ask for
				// again, and asking is re-entering the step that posts. Without
				// the edge the transition is refused and the retry the step
				// exists for never happens.
				To:      []nodeStep{stepPosting, stepAwaitingWorker},
				OnEnter: deadline[nodeStep](resolvingDeadline),
			},
			stepAwaitingWorker: {
				To:      []nodeStep{stepCheckingHost},
				OnEnter: deadline[nodeStep](awaitingWorkerDeadline),
			},
			stepAdopting: {OnEnter: deadline[nodeStep](adoptingDeadline)},
		},
	}
}

// initialDeadlines are the budgets of the step each action's machine is born in.
// A machine is already in its initial state when it is built, so that state's
// OnEnter never runs and the graph's deadline for it is never set. Setting it
// explicitly is what stops the first step of every operation from being the one
// step that cannot time out.
var initialDeadlines = map[statemachine.Action]time.Duration{
	action(simplyblockv1alpha2.StorageNodeOpsActionShutdown):        requestingDeadline,
	action(simplyblockv1alpha2.StorageNodeOpsActionRestart):         requestingDeadline,
	action(simplyblockv1alpha2.StorageNodeOpsActionSuspend):         requestingDeadline,
	action(simplyblockv1alpha2.StorageNodeOpsActionResume):          requestingDeadline,
	action(simplyblockv1alpha2.StorageNodeOpsActionRemove):          validatingDeadline,
	action(simplyblockv1alpha2.StorageNodeOpsActionMigrate):         preparingDeadline,
	action(simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance): holdingDeadline,
}

// stepBudgets is what each step's deadline was set from, which is the other half
// of the arithmetic that measures how long a step took. It is derived from the
// graph's deadlines rather than restated, so a budget changed in one place moves
// both.
var stepBudgets = map[step]time.Duration{
	stepRequesting:       requestingDeadline,
	stepAwaiting:         awaitingDeadline,
	stepValidating:       validatingDeadline,
	stepSuspending:       suspendingDeadline,
	stepMigratingVolumes: migratingDeadline,
	stepVerifying:        verifyingDeadline,
	stepRemoving:         removingDeadline,
	stepPreparing:        preparingDeadline,
	stepRelocating:       relocatingDeadline,
	stepAwaitingNode:     nodeRestartDeadline,
	stepPromoting:        promotingDeadline,
	stepHolding:          holdingDeadline,
	stepShuttingDown:     suspendingDeadline,
	stepReleasing:        releasingDeadline,
	stepAwaitingHost:     awaitingHostDeadline,
	stepRestarting:       nodeRestartDeadline,
	stepCleanup:          cleanupDeadline,
}

// Which steps an abort stops cleanly is declared on the states above, because
// the line it draws is a property of the step rather than of this kind: whether
// anything is currently down or half-done. Promoting is the clearest refusal.
// The promote has activated the target host's devices, failed and migrated the
// origin's, started a rebalance, and re-homed the logical volumes, so there is
// nothing to unwind and the operation is what finishes the relocation (§9).
//
// It is a property of the state rather than an edge to a terminal one, because a
// terminal Aborted step would be an eighteenth value in the API and the phase
// already carries that meaning.

// UnabortableSteps are the declared steps an abort cannot be honored from,
// sorted.
//
// It is exported for the DELETE guard on this kind, which asks the same question
// this package's unwind asks: a deletion may not express something spec.abort
// could not, so both channels read one graph (design-crd-model.md §3.1). The
// guard has a step out of a status and no machine, which is the whole reason
// this reads the graphs rather than the machine the reconciler holds.
func UnabortableSteps() []step {
	return statemachine.UnabortableMultiStates(graphs())
}

// unwinds reports whether an abort or a failure from this step owes the node a
// resume before the operation ends. Everything from Suspending onward in a drain
// does: the node is not serving, and an operation that stopped there and left it
// that way would take capacity out of the cluster indefinitely (§8.3).
func unwinds(current step) bool {
	switch current {
	case stepSuspending, stepMigratingVolumes, stepVerifying, stepRemoving:
		return true
	default:
		return false
	}
}

// action converts the API's action enum into the MultiConfig's key. The
// conversion exists because statemachine.Action is a concrete string type rather
// than a second type parameter, and doing it in one place keeps the graph literal
// readable.
func action(a simplyblockv1alpha2.StorageNodeOpsAction) statemachine.Action {
	return statemachine.Action(a)
}
