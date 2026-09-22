// The graphs as data: what they declare, and the three places that have to agree
// about it.
//
// A shared statemachine.KubeSnapshot cannot carry an Enum marker, so the step
// values live in three places — the graph, the kind's Enum marker, and the CEL
// rule on status.step — and nothing but a test makes them agree
// (design-storagenode.md §6.3). These are that test.
//
// The rest is what declaring a graph as data buys: a transition table is cheap to
// exercise exhaustively, so every illegal edge is a unit test rather than a review
// comment, and the refusal to abort a Promoting migration is one assertion rather
// than a code path.

package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// everyStep is the Enum marker's list, transcribed. It is written out rather than
// derived so that the assertion below compares two independent statements of the
// same set: deriving it from the graph would make the test agree with itself.
var everyStep = []string{
	"Awaiting", "AwaitingHost", "AwaitingNode", "Cleanup", "Holding",
	"MigratingVolumes", "Preparing", "Promoting", "Relocating", "Releasing",
	"Removing", "Requesting", "Restarting", "ShuttingDown", "Suspending",
	"Validating", "Verifying",
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
// compared both ways, because a rule that names a step no graph declares admits a
// status no controller can resume from.
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

// The entity's own machine carries the same three-way agreement, over a Config
// rather than a MultiConfig because a StorageNode has no spec.action to key one
// on.
func TestTheNodeStepEnumAndRuleCoverTheProvisioningGraph(t *testing.T) {
	declared := statemachine.DeclaredStates(provisioningGraph())
	want := []string{
		"Adopting", "AwaitingSlot", "AwaitingWorker", "CheckingConfig", "CheckingHost",
		"Posting", "Resolving",
	}
	if diff := cmp.Diff(want, declared); diff != "" {
		t.Errorf("the provisioning graph and the Enum marker disagree (-marker +graph):\n%s", diff)
	}
	for _, state := range declared {
		if !strings.Contains(nodeStepCELRule, "'"+state+"'") {
			t.Errorf("status.step's CEL rule does not accept the declared step %q", state)
		}
	}
	for _, named := range celRuleValues(nodeStepCELRule) {
		if !slices.Contains(declared, named) {
			t.Errorf("status.step's CEL rule accepts %q, which the graph does not declare", named)
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

// The two rules as the types declare them. Keeping a copy here is the cost of a
// rule living in a struct tag; the tests above are what make the copies worth
// having.
const opsStepCELRule = "!has(self.state) || self.state in " +
	"['Requesting','Awaiting','Validating','Suspending','MigratingVolumes','Verifying'," +
	"'Removing','Preparing','Relocating','AwaitingNode','Promoting','Holding'," +
	"'ShuttingDown','Releasing','AwaitingHost','Restarting','Cleanup']"

const nodeStepCELRule = "!has(self.state) || self.state in " +
	"['CheckingHost','CheckingConfig','AwaitingSlot','Posting','Resolving','Adopting'," +
	"'AwaitingWorker']"

// Every action the API accepts needs a graph, or an operation of that action fails
// at its first pass with ErrUnknownAction rather than doing anything.
func TestEveryActionDeclaresAGraph(t *testing.T) {
	declared := graphs()
	for _, a := range []simplyblockv1alpha2.StorageNodeOpsAction{
		simplyblockv1alpha2.StorageNodeOpsActionShutdown,
		simplyblockv1alpha2.StorageNodeOpsActionRestart,
		simplyblockv1alpha2.StorageNodeOpsActionSuspend,
		simplyblockv1alpha2.StorageNodeOpsActionResume,
		simplyblockv1alpha2.StorageNodeOpsActionRemove,
		simplyblockv1alpha2.StorageNodeOpsActionMigrate,
		simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance,
	} {
		if _, ok := declared[action(a)]; !ok {
			t.Errorf("action %s declares no graph", a)
		}
		if _, ok := initialDeadlines[action(a)]; !ok {
			t.Errorf("action %s has no budget for the step its machine is born in", a)
		}
	}
}

// The line abortability draws is whether anything is currently down or
// half-done. These seven are the sharpest cases and each would leave the node in
// a state nothing else drives it out of.
func TestNoStepPastThePointOfNoReturnIsAbortable(t *testing.T) {
	unabortable := statemachine.UnabortableMultiStates(graphs())
	for _, state := range []step{
		// The promote has re-homed the logical volumes: there is nothing to
		// unwind, and the operation is what finishes the relocation.
		stepPromoting,
		// The node is mid-restart on a host it is being moved to, and this
		// operation is the only thing watching it back.
		stepRelocating,
		stepAwaitingNode,
		// The node is down for a reboot nothing else will bring it back from.
		stepShuttingDown,
		stepReleasing,
		stepAwaitingHost,
		stepRestarting,
	} {
		if !slices.Contains(unabortable, state) {
			t.Errorf("step %q is abortable and the node is not in a state an abort can leave it in", state)
		}
	}
}

// The other half of the same statement, written out rather than derived. The
// graphs are now the only place abortability is declared, so a step that quietly
// gains or loses it would otherwise change what an abort does with nothing
// disagreeing.
func TestTheStepsAnAbortStopsCleanly(t *testing.T) {
	unabortable := statemachine.UnabortableMultiStates(graphs())
	for _, state := range []step{
		// Nothing has been issued yet.
		stepRequesting,
		// No side effect at all, which is why an abort here is an Aborted
		// directly rather than an unwind (§8.3).
		stepValidating,
		// Past the suspend, and the unwind is the resume the graph already
		// performs on every other terminal outcome from here on.
		stepSuspending,
		stepMigratingVolumes,
		stepVerifying,
		// A target host has been labeled and nothing more.
		stepPreparing,
		// The window before the node is taken down for maintenance.
		stepHolding,
	} {
		if slices.Contains(unabortable, state) {
			t.Errorf("step %q refuses an abort, and nothing it has done needs finishing", state)
		}
	}
}

// Every terminal outcome from Suspending onward owes the node a resume, because a
// node past the suspend is not serving and an operation that stopped there would
// take capacity out of the cluster for as long as nobody noticed (§8.3).
func TestTheDrainStepsPastTheSuspendUnwind(t *testing.T) {
	for _, state := range []step{
		stepSuspending, stepMigratingVolumes, stepVerifying, stepRemoving,
	} {
		if !unwinds(state) {
			t.Errorf("step %q leaves the node suspended and owes it a resume", state)
		}
	}
	// Validating performs no side effect at all, which is what makes an abort
	// there an Aborted directly rather than an unwind.
	if unwinds(stepValidating) {
		t.Error("Validating touches nothing and must not issue a resume")
	}
}

func TestEveryStepHasABudget(t *testing.T) {
	for _, state := range statemachine.DeclaredMultiStates(graphs()) {
		if _, ok := stepBudgets[step(state)]; !ok {
			t.Errorf("step %q has no budget, so no duration can be measured for it", state)
		}
	}
}

// One status.step field serves all seven actions, so nothing in the API type
// prevents a Remove from reporting Promoting. The per-action graph is what makes
// that a refusal at the point of the write.
func TestAStepOfAnotherActionIsRejected(t *testing.T) {
	_, err := graphs().FromSnapshot(context.Background(),
		action(simplyblockv1alpha2.StorageNodeOpsActionRemove),
		statemachine.Snapshot[step]{State: stepPromoting})
	if err == nil {
		t.Fatal("a Remove restored into Promoting, which belongs to Migrate")
	}
}

// The drain's graph is the line §8.2 states, and its ordering is the design:
// validation before the suspend, so a drain that cannot complete never takes
// capacity out of the cluster.
func TestTheRemoveGraphValidatesBeforeItSuspends(t *testing.T) {
	assertLine(t, simplyblockv1alpha2.StorageNodeOpsActionRemove, []step{
		stepValidating, stepSuspending, stepMigratingVolumes, stepVerifying, stepRemoving,
	})
}

// Relocating and AwaitingNode are two steps because one would race, which is the
// whole reason the migration's graph is four steps rather than three.
func TestTheMigrateGraphSplitsTheRestartFromTheWait(t *testing.T) {
	assertLine(t, simplyblockv1alpha2.StorageNodeOpsActionMigrate, []step{
		stepPreparing, stepRelocating, stepAwaitingNode, stepPromoting,
	})
}

func TestTheHostMaintenanceGraphIsTheSixStepWindow(t *testing.T) {
	assertLine(t, simplyblockv1alpha2.StorageNodeOpsActionHostMaintenance, []step{
		stepHolding, stepShuttingDown, stepReleasing, stepAwaitingHost,
		stepRestarting, stepCleanup,
	})
}

// The four single-step actions share one two-step line, which is what keeps a
// change to it from landing in one of four copies.
func TestTheSingleStepActionsShareOneLine(t *testing.T) {
	for _, a := range []simplyblockv1alpha2.StorageNodeOpsAction{
		simplyblockv1alpha2.StorageNodeOpsActionShutdown,
		simplyblockv1alpha2.StorageNodeOpsActionRestart,
		simplyblockv1alpha2.StorageNodeOpsActionSuspend,
		simplyblockv1alpha2.StorageNodeOpsActionResume,
	} {
		assertLine(t, a, []step{stepRequesting, stepAwaiting})
	}
}

// assertLine walks an action's graph from its initial state and checks it is the
// straight line the design states, ending terminal.
func assertLine(t *testing.T, a simplyblockv1alpha2.StorageNodeOpsAction, want []step) {
	t.Helper()
	ctx := context.Background()

	machine, err := graphs().FromSnapshot(ctx, action(a), statemachine.Snapshot[step]{})
	if err != nil {
		t.Fatalf("build the %s machine: %v", a, err)
	}
	defer machine.Close()

	if got := machine.CurrentState(); got != want[0] {
		t.Fatalf("%s starts at %q, want %q", a, got, want[0])
	}
	for i := 1; i < len(want); i++ {
		if err := machine.TransitionTo(ctx, want[i]); err != nil {
			t.Fatalf("%s cannot move from %q to %q: %v", a, want[i-1], want[i], err)
		}
	}
	if !machine.IsTerminal() {
		t.Errorf("%s does not end at %q", a, want[len(want)-1])
	}
}

// The provisioning machine branches rather than running in a line: adoption
// diverts from the host check, from the configuration gate, and from the queue for
// a node-add slot alike, which is what lets an upgrade Secret and a backend node
// found at the worker's address reach the same step from anywhere before the add.
//
// The queue is the one that matters after a restart. A backend node can appear
// while an object waits there, and without the edge the only answer to one that
// has is a second add.
func TestAdoptionIsReachableFromEveryGateBeforeTheAdd(t *testing.T) {
	ctx := context.Background()
	for _, from := range []nodeStep{stepCheckingHost, stepCheckingConfig, stepAwaitingSlot} {
		config := provisioningGraph()
		machine, err := statemachine.NewFromSnapshot(ctx, config,
			statemachine.Snapshot[nodeStep]{State: from})
		if err != nil {
			t.Fatalf("build the provisioning machine at %q: %v", from, err)
		}
		if err := machine.TransitionTo(ctx, stepAdopting); err != nil {
			t.Errorf("adoption is not reachable from %q: %v", from, err)
		}
		machine.Close()
	}
}

// The second socket of a worker must not post again: its sibling's claim is what
// it reads, and it enters Resolving directly. That edge is what makes the skip
// expressible at all.
func TestAwaitingSlotMayGoStraightToResolving(t *testing.T) {
	ctx := context.Background()
	machine, err := statemachine.NewFromSnapshot(ctx, provisioningGraph(),
		statemachine.Snapshot[nodeStep]{State: stepAwaitingSlot})
	if err != nil {
		t.Fatalf("build the provisioning machine: %v", err)
	}
	defer machine.Close()

	if err := machine.TransitionTo(ctx, stepResolving); err != nil {
		t.Errorf("a sibling's claim cannot be read as a slot already posted: %v", err)
	}
}
