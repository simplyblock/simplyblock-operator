// The state graph of every StorageClusterOps action, declared as data.
//
// One status.step field serves all seven actions, so nothing in the API type
// prevents an Activate from reporting Rebalancing. The per-action graph makes
// that an IllegalTransitionError at the point of the write rather than an
// accepted status (design-crd-model.md §3.1), which is the whole reason the
// steps are declared here instead of switched on in the reconciler.
//
// MultiConfig validates every declared graph whenever a machine is built for
// any of them, so a bad edge in RollingRestart — the action least often
// exercised and the most expensive to exercise — is caught by any test that
// builds a machine at all.
//
// Five actions are one call and one wait and share a two-step line. Restart is
// the one action with two side effects, because the control plane has no
// cluster restart endpoint. RollingRestart's graph covers one node, and the
// walk across the fleet is status.rollingRestart beside it: starting the next
// node is Machine.Reset rather than an edge, so Rebalancing stays terminal and
// IsTerminal answers "this node is done."
//
// design-storagecluster.md §5.3, §6.3, §6.4, and §7 are the specification.

package cluster

import (
	"context"
	"time"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// step is the kind's step type, aliased so the graph literals below read as the
// graphs rather than as a wall of package qualifiers.
type step = simplyblockv1alpha2.StorageClusterOpsStep

const (
	stepRequesting       = simplyblockv1alpha2.StorageClusterOpsStepRequesting
	stepAwaiting         = simplyblockv1alpha2.StorageClusterOpsStepAwaiting
	stepShuttingDown     = simplyblockv1alpha2.StorageClusterOpsStepShuttingDown
	stepStarting         = simplyblockv1alpha2.StorageClusterOpsStepStarting
	stepCheckingPeers    = simplyblockv1alpha2.StorageClusterOpsStepCheckingPeers
	stepShuttingDownNode = simplyblockv1alpha2.StorageClusterOpsStepShuttingDownNode
	stepRefreshingPod    = simplyblockv1alpha2.StorageClusterOpsStepRefreshingPod
	stepAwaitingPod      = simplyblockv1alpha2.StorageClusterOpsStepAwaitingPod
	stepRestartingNode   = simplyblockv1alpha2.StorageClusterOpsStepRestartingNode
	stepRebalancing      = simplyblockv1alpha2.StorageClusterOpsStepRebalancing
)

// How long each step may take before it is reported as stuck.
//
// They differ by what the step is waiting on rather than by preference. A
// request is one HTTP call. Awaiting a cluster to come back is a data-plane
// operation across every node. CheckingPeers can hold indefinitely by design
// (§7.2), so its budget is generous but finite: what the deadline separates is
// a walk holding because the cluster is degraded from one holding because of a
// bug, and without one the two look identical.
const (
	requestingDeadline    = 2 * time.Minute
	awaitingDeadline      = 30 * time.Minute
	checkingPeersDeadline = 2 * time.Hour
	nodeShutdownDeadline  = 30 * time.Minute
	podRefreshDeadline    = 10 * time.Minute
	nodeRestartDeadline   = 45 * time.Minute
	rebalancingDeadline   = 4 * time.Hour
)

// graphs declares one state graph per action over one step type.
func graphs() statemachine.MultiConfig[step] {
	deadline := func(d time.Duration) statemachine.TransitionFunc[step] {
		return func(context.Context, step, step) (time.Duration, error) { return d, nil }
	}

	// requestAndWait is the two-step line five actions share: post the action,
	// then wait for the completion condition. It is a function rather than a
	// shared value because MultiConfig copies the graph when a machine is
	// built and a shared map would be one graph under five keys.
	requestAndWait := func() statemachine.Config[step] {
		return statemachine.Config[step]{
			Initial: stepRequesting,
			States: map[step]statemachine.StateDef[step]{
				// Requesting has issued nothing. Awaiting is past the call, and
				// a cluster told to shut down is shutting down whatever this
				// object says.
				stepRequesting: {
					To:        []step{stepAwaiting},
					Abortable: true,
					OnEnter:   deadline(requestingDeadline),
				},
				stepAwaiting: {OnEnter: deadline(awaitingDeadline)},
			},
		}
	}

	return statemachine.MultiConfig[step]{
		action(simplyblockv1alpha2.StorageClusterOpsActionActivate):   requestAndWait(),
		action(simplyblockv1alpha2.StorageClusterOpsActionExpand):     requestAndWait(),
		action(simplyblockv1alpha2.StorageClusterOpsActionShutdown):   requestAndWait(),
		action(simplyblockv1alpha2.StorageClusterOpsActionStart):      requestAndWait(),
		action(simplyblockv1alpha2.StorageClusterOpsActionCancelTask): requestAndWait(),

		// Restart sequences a shutdown and a start, because the control plane
		// offers no restart of its own (§9). A server-side one would collapse
		// this graph to the line above.
		action(simplyblockv1alpha2.StorageClusterOpsActionRestart): {
			Initial: stepShuttingDown,
			States: map[step]statemachine.StateDef[step]{
				stepShuttingDown: {To: []step{stepStarting}, OnEnter: deadline(awaitingDeadline)},
				stepStarting:     {OnEnter: deadline(awaitingDeadline)},
			},
		},

		// One machine lifetime per node. RefreshingPod and AwaitingPod are
		// entered only when spec.rollingRestart.refreshSNodeAPI is set, which
		// is why ShuttingDownNode declares an edge past them as well as into
		// them.
		action(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart): {
			Initial: stepCheckingPeers,
			States: map[step]statemachine.StateDef[step]{
				// CheckingPeers performs no side effect and is the step before
				// the walk touches a node. From ShuttingDownNode to
				// RestartingNode the node is offline and this operation is the
				// only thing that will bring it back, so an abort honored there
				// would leave a storage node down with nothing driving it up.
				// Rebalancing is after the node is back and the cluster is
				// settling on its own, which it finishes whether or not this
				// operation is watching.
				stepCheckingPeers: {
					To:        []step{stepShuttingDownNode},
					Abortable: true,
					OnEnter:   deadline(checkingPeersDeadline),
				},
				stepShuttingDownNode: {
					To:      []step{stepRefreshingPod, stepRestartingNode},
					OnEnter: deadline(nodeShutdownDeadline),
				},
				stepRefreshingPod: {
					To:      []step{stepAwaitingPod},
					OnEnter: deadline(podRefreshDeadline),
				},
				stepAwaitingPod: {
					To:      []step{stepRestartingNode},
					OnEnter: deadline(podRefreshDeadline),
				},
				stepRestartingNode: {
					To:      []step{stepRebalancing},
					OnEnter: deadline(nodeRestartDeadline),
				},
				// Terminal, so IsTerminal answers "this node is done" and the
				// machine never carries a cycle. Starting the next node is
				// Machine.Reset, which returns to CheckingPeers, clears the
				// deadline, validates no edge, and runs no hook.
				stepRebalancing: {Abortable: true, OnEnter: deadline(rebalancingDeadline)},
			},
		},
	}
}

// initialDeadlines are the budgets of the step each action's machine is born
// in. A machine is already in its initial state when it is built, so that
// state's OnEnter never runs and the graph's deadline for it is never set.
// Setting it explicitly is what stops the first step of every operation from
// being the one step that cannot time out.
var initialDeadlines = map[statemachine.Action]time.Duration{
	action(simplyblockv1alpha2.StorageClusterOpsActionActivate):       requestingDeadline,
	action(simplyblockv1alpha2.StorageClusterOpsActionExpand):         requestingDeadline,
	action(simplyblockv1alpha2.StorageClusterOpsActionShutdown):       requestingDeadline,
	action(simplyblockv1alpha2.StorageClusterOpsActionStart):          requestingDeadline,
	action(simplyblockv1alpha2.StorageClusterOpsActionCancelTask):     requestingDeadline,
	action(simplyblockv1alpha2.StorageClusterOpsActionRestart):        awaitingDeadline,
	action(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart): checkingPeersDeadline,
}

// Which steps an abort stops cleanly is declared on the states above, because
// the line it draws is a property of the step rather than of this kind: whether
// anything is currently down or half-done. Awaiting, ShuttingDown, and Starting
// are that rule at cluster scale, and the rolling restart's four middle steps
// are the sharpest case of it at node scale.
//
// It is a property of the state rather than an edge to a terminal one, because a
// terminal Aborted step would be an eleventh value in the API and the phase
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

// action converts the API's action enum into the MultiConfig's key. The
// conversion exists because statemachine.Action is a concrete string type
// rather than a second type parameter (see its doc comment), and doing it in
// one place keeps the graph literal readable.
func action(a simplyblockv1alpha2.StorageClusterOpsAction) statemachine.Action {
	return statemachine.Action(a)
}
