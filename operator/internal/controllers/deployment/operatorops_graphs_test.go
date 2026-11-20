// The discovery graph as data, and the three places that have to agree about
// what its steps are.
//
// A shared statemachine.KubeSnapshot cannot carry an Enum marker, so the step
// values live in the graph, in OperatorOpsStep's own Enum marker, and in the CEL
// rule on status.step — and nothing but a test makes the three agree
// (design-crd-model.md §3.1). These are that test, and they are the reason the
// graph is worth declaring as data at all: a transition table is cheap to
// exercise exhaustively, so an illegal edge is a unit test rather than a review
// comment.

package deployment

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// everyDiscoveryStep is the Enum marker's list, transcribed. It is written out
// rather than derived so the assertion compares two independent statements of
// the same set: deriving it from the graph would make the test agree with
// itself.
var everyDiscoveryStep = []string{"Inspecting", "Probing", "Writing"}

// discoveryStepCELRule is the rule as OperatorOpsStatus declares it. Keeping a
// copy here is the only way to compare it against anything: it is a literal in a
// struct tag, and no marker reaches a field of a type another module declares.
const discoveryStepCELRule = "!has(self.state) || self.state in ['Inspecting','Probing','Writing']"

func TestTheDiscoveryStepEnumCoversEveryDeclaredState(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(operatorOpsGraphs())
	want := slices.Clone(everyDiscoveryStep)
	slices.Sort(want)
	if diff := cmp.Diff(want, declared); diff != "" {
		t.Errorf("the graph and the Enum marker disagree (-marker +graph):\n%s", diff)
	}
}

// The rule is compared both ways. One that named a step no graph declares would
// admit a status no reconcile can resume from, and one that omitted a declared
// step would refuse a status the controller itself writes.
func TestTheDiscoveryCELRuleCoversEveryDeclaredState(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(operatorOpsGraphs())
	for _, state := range declared {
		if !strings.Contains(discoveryStepCELRule, "'"+state+"'") {
			t.Errorf("status.step's CEL rule does not accept the declared step %q", state)
		}
	}
	for _, named := range quotedValues(discoveryStepCELRule) {
		if !slices.Contains(declared, named) {
			t.Errorf("status.step's CEL rule accepts %q, which no graph declares", named)
		}
	}
}

// quotedValues reads the quoted values out of a rule's `in` list.
func quotedValues(rule string) []string {
	var values []string
	for _, part := range strings.Split(rule, "'") {
		if part != "" && !strings.ContainsAny(part, "[],| ") {
			values = append(values, part)
		}
	}
	return values
}

// Every action the API accepts needs a graph, or an operation of that action
// fails at its first pass rather than doing anything.
func TestEveryDiscoveryActionDeclaresAGraph(t *testing.T) {
	declared := operatorOpsGraphs()
	for _, action := range []simplyblockv1alpha2.OperatorOpsAction{
		simplyblockv1alpha2.OperatorOpsActionDiscover,
	} {
		if _, ok := declared[statemachine.Action(action)]; !ok {
			t.Errorf("the API accepts action %q and no graph declares it", action)
		}
	}
	for action := range declared {
		if string(action) != string(simplyblockv1alpha2.OperatorOpsActionDiscover) {
			t.Errorf("a graph declares action %q, which the API does not accept", action)
		}
	}
}

// The graph is a line, and the line is what the run's ordering rests on:
// Inspecting settles which workers the run is about, Probing reads only those,
// and Writing turns what they reported into a document. An edge that skipped
// Inspecting would draft a fleet nobody listed.
func TestTheDiscoveryGraphIsTheLineTheRunWalks(t *testing.T) {
	graph, ok := operatorOpsGraphs()[actionDiscover]
	if !ok {
		t.Fatal("no graph is declared for Discover")
	}

	if graph.Initial != stepInspecting {
		t.Errorf("the run begins at %q, want Inspecting", graph.Initial)
	}

	want := map[simplyblockv1alpha2.OperatorOpsStep][]simplyblockv1alpha2.OperatorOpsStep{
		stepInspecting: {stepProbing},
		stepProbing:    {stepWriting},
		stepWriting:    nil,
	}
	for from, to := range want {
		state, declared := graph.States[from]
		if !declared {
			t.Errorf("step %q is not in the graph", from)
			continue
		}
		if diff := cmp.Diff(to, state.To); diff != "" {
			t.Errorf("the edges out of %q disagree (-want +graph):\n%s", from, diff)
		}
	}
}

// Every step of a discovery run is abortable, because the run changes nothing it
// has to take back: it reads nodes, creates Jobs that carry its owner reference,
// and creates one document at the very end. That is why the kind gets no DELETE
// admission guard (design-crd-model.md §3.1), and the guard's absence is only
// correct for as long as this holds.
func TestEveryDiscoveryStepIsAbortable(t *testing.T) {
	if unabortable := statemachine.UnabortableMultiStates(operatorOpsGraphs()); len(unabortable) > 0 {
		t.Errorf("steps %v cannot be aborted, so a delete arriving in one of them has "+
			"something to unwind and there is no webhook refusing it", unabortable)
	}
}
