// The state graph of each StorageDeviceOps action, declared as data.
//
//	Restart  Requesting ──► Awaiting
//	Fail     Removing ──► Failing ──► Awaiting
//
// Two actions are declared, because two are what the control plane's v2 API can
// serve. The other three of design-storagedevice.md §6 are blocked on verbs it
// does not offer, and the TODO beside their constants in
// storagedeviceops_types.go is the list.
//
// **Fail is three steps where §6 specifies two.** The control plane refuses to
// fail a device that is still in the data path, so the removal §6 describes as
// the action's effect is a call of its own that precedes it. Splitting them is
// what the write-ahead rule asks for in any case: two side effects in one step
// are two a resumed operation cannot tell apart.
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
	stepDeviceRemoving   = simplyblockv1alpha2.StorageDeviceOpsStepRemoving
	stepDeviceFailing    = simplyblockv1alpha2.StorageDeviceOpsStepFailing
)

// The MultiConfig keys for the actions this kind performs.
const (
	actionDeviceRestart = statemachine.Action(simplyblockv1alpha2.StorageDeviceOpsActionRestart)
	actionDeviceFail    = statemachine.Action(simplyblockv1alpha2.StorageDeviceOpsActionFail)
)

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

	// removingDeviceDeadline bounds the removal. It is larger than the restart's
	// budget because the call disconnects the device from every node in the
	// cluster before it answers, so the work behind it grows with the cluster
	// rather than with the device.
	removingDeviceDeadline = 10 * time.Minute

	// awaitingFailureDeadline bounds the control plane reporting a failure it
	// records inside the call that asked for it. What is waited on is the
	// device stream catching up, not a rebuild: the rebuild is started by the
	// failure and outlives the operation, and an operation that waited for it
	// would hold the device's lock for hours after its decision had landed.
	awaitingFailureDeadline = 5 * time.Minute
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
		actionDeviceFail: {
			Initial: stepDeviceRemoving,
			States: map[deviceStep]statemachine.StateDef[deviceStep]{
				// Removing is abortable for the same reason Requesting is:
				// nothing has been issued while the operation sits in it. The
				// reconciler asks one question the graph cannot, because the
				// call is made inside this step: a resumed operation whose
				// device is already removed is refused the edge the graph
				// grants it.
				stepDeviceRemoving: {
					To:        []deviceStep{stepDeviceFailing},
					Abortable: true,
					OnEnter:   deviceDeadline(removingDeviceDeadline),
				},
				// Failing is not. The device is out of the data path by the
				// time this step is entered, and this operator has no call that
				// puts it back. An abort here would record a stop while leaving
				// the device removed under an object that says nothing
				// happened.
				stepDeviceFailing: {
					To:      []deviceStep{stepDeviceAwaiting},
					OnEnter: deviceDeadline(requestingDeviceDeadline),
				},
				// Awaiting is not, and here the reason is stronger than the
				// restart's: a failure is the one decision in this kind that
				// nothing reverses.
				stepDeviceAwaiting: {OnEnter: deviceDeadline(awaitingFailureDeadline)},
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

// initialDeviceDeadlines are the budgets of the step each action's machine is
// born in. A machine is already in its initial state when it is built, so that
// state's OnEnter never runs and the graph's deadline for it is never set.
// Setting it explicitly is what stops the first step of every operation from
// being the one step that cannot time out.
var initialDeviceDeadlines = map[statemachine.Action]time.Duration{
	actionDeviceRestart: requestingDeviceDeadline,
	actionDeviceFail:    removingDeviceDeadline,
}

// UnabortableDeviceSteps are the steps a running operation cannot be stopped in,
// which is the refusal table a DELETE admission guard would derive from this
// graph (design-crd-model.md §3.1).
//
// It is exported so that the guard and the graph cannot come to disagree: the
// webhook's table is checked against this rather than transcribed from it.
func UnabortableDeviceSteps() []deviceStep {
	return statemachine.UnabortableMultiStates(storageDeviceOpsGraphs())
}
