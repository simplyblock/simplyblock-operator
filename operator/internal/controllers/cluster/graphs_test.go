// The graphs as data: what they declare, and the three places that have to
// agree about it.
//
// A shared statemachine.KubeSnapshot cannot carry an Enum marker, so the step
// values live in three places — the graph, the kind's Enum marker, and the CEL
// rule on status.step — and nothing but a test makes them agree
// (design-storagecluster.md §5.3). These are that test.
//
// The rest is what declaring a graph as data buys: a transition table is
// cheap to exercise exhaustively, so every illegal edge is a unit test rather
// than a review comment.

package cluster

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// everyStep is the Enum marker's list, transcribed. It is written out rather
// than derived so that the assertion below compares two independent statements
// of the same set: deriving it from the graph would make the test agree with
// itself.
var everyStep = []string{
	"Awaiting", "AwaitingPod", "CheckingPeers", "Rebalancing", "RefreshingPod",
	"Requesting", "RestartingNode", "ShuttingDown", "ShuttingDownNode", "Starting",
}

func TestTheStepEnumCoversEveryDeclaredState(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(graphs())
	want := slices.Clone(everyStep)
	slices.Sort(want)
	if diff := cmp.Diff(want, declared); diff != "" {
		t.Errorf("the graphs and the Enum marker disagree (-marker +graphs):\n%s", diff)
	}
}

// The CEL rule on status.step is what an Enum marker would do if a marker could
// reach a field of a type another module declares. It is a literal list in a
// struct tag, so nothing but this compares it against the graph — and it is
// compared both ways, because a rule that names a step no graph declares
// admits a status no controller can resume from.
func TestTheCELRuleCoversEveryDeclaredState(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(graphs())
	for _, state := range declared {
		if !strings.Contains(opsStepCELRule, "'"+state+"'") {
			t.Errorf("status.step's CEL rule does not accept the declared step %q", state)
		}
	}
	for _, named := range celRuleValues(opsStepCELRule) {
		if !slices.Contains(declared, named) {
			t.Errorf("status.step's CEL rule accepts %q, which no graph declares", named)
		}
	}
}

// celRuleValues reads the quoted values out of the rule's `in` list.
func celRuleValues(rule string) []string {
	var values []string
	for _, part := range strings.Split(rule, "'") {
		if part != "" && !strings.ContainsAny(part, "[],| ") {
			values = append(values, part)
		}
	}
	return values
}

// opsStepCELRule is the rule as the type declares it. Keeping a copy here is
// the cost of the rule living in a struct tag; the test above is what makes the
// copy worth having.
const opsStepCELRule = "!has(self.state) || self.state in " +
	"['Requesting','Awaiting','ShuttingDown','Starting','CheckingPeers'," +
	"'ShuttingDownNode','RefreshingPod','AwaitingPod','RestartingNode','Rebalancing']"

// Every action the API accepts needs a graph, or an operation of that action
// fails at its first pass with ErrUnknownAction rather than doing anything.
func TestEveryActionDeclaresAGraph(t *testing.T) {
	declared := graphs()
	for _, a := range []simplyblockv1alpha2.StorageClusterOpsAction{
		simplyblockv1alpha2.StorageClusterOpsActionActivate,
		simplyblockv1alpha2.StorageClusterOpsActionExpand,
		simplyblockv1alpha2.StorageClusterOpsActionShutdown,
		simplyblockv1alpha2.StorageClusterOpsActionStart,
		simplyblockv1alpha2.StorageClusterOpsActionRestart,
		simplyblockv1alpha2.StorageClusterOpsActionRollingRestart,
		simplyblockv1alpha2.StorageClusterOpsActionCancelTask,
	} {
		if _, ok := declared[action(a)]; !ok {
			t.Errorf("action %s declares no graph", a)
		}
		if _, ok := initialDeadlines[action(a)]; !ok {
			t.Errorf("action %s has no budget for the step its machine is born in", a)
		}
	}
}

// An abortable step that no graph declares is a table that has drifted from the
// graphs beside it, and the drift is silent: an abort would simply never be
// honored from it.
func TestEveryAbortableStepIsDeclared(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(graphs())
	for s := range abortableSteps {
		if !slices.Contains(declared, string(s)) {
			t.Errorf("abortableSteps names %q, which no graph declares", s)
		}
	}
}

// Every step needs a budget in stepBudgets, or its duration is never measured
// and a slow operation stays anecdotal.
func TestEveryStepHasABudget(t *testing.T) {
	for _, s := range statemachine.DeclaredMultiStates(graphs()) {
		if _, ok := stepBudgets[step(s)]; !ok {
			t.Errorf("step %q has no entry in stepBudgets", s)
		}
	}
}

// The union stops being a union. One status.step field serves seven actions, so
// nothing in the type prevents an Activate from reporting Rebalancing; the
// per-action graph makes that an error at the point of the write.
func TestAStepOfAnotherActionIsRejected(t *testing.T) {
	ctx := context.Background()
	machine, err := graphs().New(ctx, action(simplyblockv1alpha2.StorageClusterOpsActionActivate))
	if err != nil {
		t.Fatalf("build the Activate machine: %v", err)
	}
	defer machine.Close()

	if err := machine.TransitionTo(ctx, stepRebalancing); err == nil {
		t.Fatal("an Activate accepted a step that belongs only to the rolling restart")
	}
}

// The rolling restart's graph is a line per node with one branch, and
// Rebalancing is terminal so that IsTerminal answers "this node is done"
// rather than "the operation is done."
func TestTheRollingRestartGraphIsAcyclicAndTerminatesAtRebalancing(t *testing.T) {
	ctx := context.Background()
	machine, err := graphs().New(ctx,
		action(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))
	if err != nil {
		t.Fatalf("build the RollingRestart machine: %v", err)
	}
	defer machine.Close()

	walk := []step{stepCheckingPeers, stepShuttingDownNode, stepRefreshingPod,
		stepAwaitingPod, stepRestartingNode, stepRebalancing}
	if got := machine.CurrentState(); got != walk[0] {
		t.Fatalf("the machine starts at %q, want %q", got, walk[0])
	}
	for _, next := range walk[1:] {
		if err := machine.TransitionTo(ctx, next); err != nil {
			t.Fatalf("enter %q: %v", next, err)
		}
	}
	if !machine.IsTerminal() {
		t.Error("Rebalancing is not terminal, so IsTerminal cannot mean the node is done")
	}

	// Reset is what starts the next node. It is not an edge, which is what
	// keeps the graph acyclic.
	machine.Reset()
	if got := machine.CurrentState(); got != stepCheckingPeers {
		t.Errorf("after Reset the machine is at %q, want CheckingPeers", got)
	}
	if _, bounded := machine.Deadline(); bounded {
		t.Error("Reset left a deadline armed, so the next node inherits the last one's budget")
	}
}

// Skipping the pod refresh is a declared edge rather than an improvisation, so
// an operation that did not ask for it can still reach RestartingNode.
func TestShuttingDownANodeMayGoStraightToRestarting(t *testing.T) {
	ctx := context.Background()
	machine, err := graphs().New(ctx,
		action(simplyblockv1alpha2.StorageClusterOpsActionRollingRestart))
	if err != nil {
		t.Fatalf("build the RollingRestart machine: %v", err)
	}
	defer machine.Close()

	if err := machine.TransitionTo(ctx, stepShuttingDownNode); err != nil {
		t.Fatalf("enter ShuttingDownNode: %v", err)
	}
	if err := machine.TransitionTo(ctx, stepRestartingNode); err != nil {
		t.Fatalf("a walk that skips the pod refresh cannot reach RestartingNode: %v", err)
	}
}

// A Restart is two side effects in sequence, because the control plane offers
// no restart of its own.
func TestRestartSequencesAShutdownAndAStart(t *testing.T) {
	ctx := context.Background()
	machine, err := graphs().New(ctx, action(simplyblockv1alpha2.StorageClusterOpsActionRestart))
	if err != nil {
		t.Fatalf("build the Restart machine: %v", err)
	}
	defer machine.Close()

	if got := machine.CurrentState(); got != stepShuttingDown {
		t.Fatalf("the machine starts at %q, want ShuttingDown", got)
	}
	if err := machine.TransitionTo(ctx, stepStarting); err != nil {
		t.Fatalf("enter Starting: %v", err)
	}
	if !machine.IsTerminal() {
		t.Error("Starting is not terminal, so a restart never finishes")
	}
}
