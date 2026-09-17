// The state graph of each StorageDeviceOps action, declared as data.
//
//	Requesting ──► Awaiting
//
// One action is declared, because one is what the control plane's v2 API can
// serve; the other four of design-storagedevice.md §6 are blocked on verbs it
// does not offer, and v1alpha2.ExternalDependencies is the list. A MultiConfig
// with one entry is what every other Ops controller in this group uses, so the
// second action arrives as an entry rather than as a case.
//
// design-storagedevice.md §6 is the specification.

package node

import (
	"context"
	"time"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// deviceStep is the operation's step type, aliased so the graph literal reads as
// the graph.
type deviceStep = simplyblockv1alpha2.StorageDeviceOpsStep

const (
	stepDeviceRequesting = simplyblockv1alpha2.StorageDeviceOpsStepRequesting
	stepDeviceAwaiting   = simplyblockv1alpha2.StorageDeviceOpsStepAwaiting
)

// actionDeviceRestart is the MultiConfig key for the one action this kind
// performs.
const actionDeviceRestart = statemachine.Action(simplyblockv1alpha2.StorageDeviceOpsActionRestart)

// How long each step may take before the operation is reported as stuck.
const (
	// requestingDeviceDeadline bounds one POST to the control plane. A step
	// still waiting after this is one whose control plane is not answering,
	// which is a different problem from a device that will not come back.
	requestingDeviceDeadline = 2 * time.Minute

	// awaitingDeviceDeadline bounds the device returning to service. A restart
	// is a controller reset and a re-probe rather than a rebuild, so it is
	// minutes; a device still absent after this is one that did not survive
	// being recycled, which is the outcome worth reporting rather than waiting
	// out.
	awaitingDeviceDeadline = 15 * time.Minute
)

// storageDeviceOpsGraphs declares the state graph of each action.
func storageDeviceOpsGraphs() statemachine.MultiConfig[deviceStep] {
	return statemachine.MultiConfig[deviceStep]{
		actionDeviceRestart: {
			Initial: stepDeviceRequesting,
			States: map[deviceStep]statemachine.StateDef[deviceStep]{
				// Requesting is abortable because nothing has been issued yet:
				// the step's side effect happens on the pass after the step is
				// persisted, so an abort arriving in it stops a call that has
				// not been made.
				stepDeviceRequesting: {
					To:        []deviceStep{stepDeviceAwaiting},
					Abortable: true,
					OnEnter:   deviceDeadline(requestingDeviceDeadline),
				},
				// Awaiting is not. The control plane has accepted the restart
				// and there is no call that recalls one, so an abort here would
				// record a stop that did not happen while the device restarted
				// anyway.
				stepDeviceAwaiting: {OnEnter: deviceDeadline(awaitingDeviceDeadline)},
			},
		},
	}
}

// deviceDeadline is the entry hook every state here carries: it sets the step's
// budget and performs nothing. The work happens on the pass that follows,
// against the step the entry's write persisted, which is what makes a crash
// between the two resumable rather than invisible.
func deviceDeadline(d time.Duration) statemachine.TransitionFunc[deviceStep] {
	return func(context.Context, deviceStep, deviceStep) (time.Duration, error) { return d, nil }
}

// initialDeviceDeadline is the budget of the step every operation is born in. A
// machine is already in its initial state when it is built, so that state's
// OnEnter never runs, and setting it explicitly is what stops the first step
// from being the one step that cannot time out.
const initialDeviceDeadline = requestingDeviceDeadline

// UnabortableDeviceSteps are the steps a running operation cannot be stopped in,
// which is the refusal table a DELETE admission guard would derive from this
// graph (design-crd-model.md §3.1).
//
// It is exported so that the guard and the graph cannot come to disagree: the
// webhook's table is checked against this rather than transcribed from it.
func UnabortableDeviceSteps() []deviceStep {
	return statemachine.UnabortableMultiStates(storageDeviceOpsGraphs())
}
