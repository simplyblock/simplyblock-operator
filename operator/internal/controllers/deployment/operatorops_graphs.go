// The state graph of each OperatorOps action, declared as data.
//
//	Inspecting ──► Probing ──► Writing
//
// The line is the run's ordering and the ordering is the point of it.
// Inspecting settles which workers the run is about, so a node joining while the
// probes run does not silently join the draft; Probing reads only those workers;
// Writing turns what they reported into a document. An edge that skipped a step
// would draft a fleet nobody listed, and declaring the edges rather than
// switching on the current step is what makes that a compile-time shape instead
// of a review comment.
//
// It is a MultiConfig although the kind carries one action. Every other Ops
// controller in this group keys its graphs on the action, and a kind that read
// differently for having one entry would make a reader check whether the
// difference meant something. A MultiConfig with one entry costs nothing, and
// the second action arrives as an entry rather than as a case.
//
// design-crd-model.md §3.1 is the specification.

package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// discoveryStep is the run's step type, aliased so the graph literal reads as
// the graph rather than as a wall of package qualifiers.
type discoveryStep = simplyblockv1alpha2.OperatorOpsStep

const (
	stepInspecting = simplyblockv1alpha2.OperatorOpsStepInspecting
	stepProbing    = simplyblockv1alpha2.OperatorOpsStepProbing
	stepWriting    = simplyblockv1alpha2.OperatorOpsStepWriting
)

// actionDiscover is the MultiConfig key for the one action this kind carries.
const actionDiscover = statemachine.Action(simplyblockv1alpha2.OperatorOpsActionDiscover)

// How long each step may take before the run is reported as stuck.
//
// They differ by what the step waits on. Inspecting and Writing are Kubernetes
// reads and one create, so neither can be slow for a reason worth waiting out.
// Probing waits on one Job per worker, which is where the time goes.
const (
	// inspectingDeadline bounds listing the cluster's nodes and the
	// StorageNodes already on them. A step still waiting after this is one
	// whose API server is not answering, which reconciling again does not fix
	// any faster than reporting it.
	inspectingDeadline = 5 * time.Minute

	// writingDeadline bounds turning the reports into a document: reading the
	// ConfigMaps the probes wrote, and one create.
	writingDeadline = 5 * time.Minute
)

// operatorOpsGraphs declares the state graph of each action.
//
// Every step is abortable, because a discovery run changes nothing it would
// have to take back: it reads nodes, creates Jobs that carry its own owner
// reference, and creates one document at the very end. That is what lets the
// kind go without the DELETE admission guard of design-crd-model.md §3.1 — the
// guard derives its refusal table from the graph, and this graph refuses
// nothing.
func operatorOpsGraphs() statemachine.MultiConfig[discoveryStep] {
	return statemachine.MultiConfig[discoveryStep]{
		actionDiscover: {
			Initial: stepInspecting,
			States: map[discoveryStep]statemachine.StateDef[discoveryStep]{
				stepInspecting: {
					To:        []discoveryStep{stepProbing},
					Abortable: true,
					OnEnter:   discoveryDeadline(inspectingDeadline),
				},
				stepProbing: {
					To:        []discoveryStep{stepWriting},
					Abortable: true,
					OnEnter:   discoveryDeadline(probingDeadline),
				},
				stepWriting: {
					Abortable: true,
					OnEnter:   discoveryDeadline(writingDeadline),
				},
			},
		},
	}
}

// discoveryDeadline is the entry hook every state here carries: it sets the
// step's budget and performs nothing. The work of a step happens on the pass
// that follows, against the step the entry's write persisted, which is what
// makes a crash between the two resumable rather than invisible.
func discoveryDeadline(d time.Duration) statemachine.TransitionFunc[discoveryStep] {
	return func(context.Context, discoveryStep, discoveryStep) (time.Duration, error) {
		return d, nil
	}
}

// initialDiscoveryDeadline is the budget of the step every run is born in.
//
// A machine is already in its initial state when it is built, so that state's
// OnEnter never runs and the graph's deadline for it is never set. Setting it
// explicitly is what stops the first step from being the one step that cannot
// time out.
const initialDiscoveryDeadline = inspectingDeadline

// discoveryTimeoutMessage says what a step outliving its deadline means for this
// run, rather than only that it happened.
//
// It is per step because what is left behind differs. Probing leaves the reports
// that did arrive in ConfigMaps labeled for the run, and a message that did not
// name them would leave a reviewer with the evidence sitting in the cluster and
// no way to know it is there.
func discoveryTimeoutMessage(step discoveryStep) string {
	switch step {
	case stepInspecting:
		return fmt.Sprintf("the cluster could not be read within %s", inspectingDeadline)
	case stepProbing:
		return fmt.Sprintf("the probes did not all finish within %s; the reports that did "+
			"arrive are in the ConfigMaps labeled for this run", probingDeadline)
	case stepWriting:
		return fmt.Sprintf("the draft could not be written within %s", writingDeadline)
	default:
		return fmt.Sprintf("step %s outlived its deadline", step)
	}
}

// nextDiscoveryStep is the step that follows the current one. The graph is a
// line, so the first edge is the only edge.
func nextDiscoveryStep(machine *statemachine.Machine[discoveryStep]) (discoveryStep, bool) {
	for next := range machine.AllowedTransitions() {
		return next, true
	}
	return machine.CurrentState(), false
}
