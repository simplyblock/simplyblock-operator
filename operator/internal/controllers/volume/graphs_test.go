// The migration's graph as data: what it declares, and the three places that
// have to agree about it.
//
// A shared statemachine.KubeSnapshot cannot carry an Enum marker, so the step
// values live in three places — the graph, the kind's Enum marker, and the CEL
// rule on status.step — and nothing but a test makes them agree.
//
// The rest is what declaring the graph as data buys: the refusal to abort a
// Verifying operation is one assertion rather than a code path, and the copy's
// deadline, which is the one bound in this package that is computed rather than
// fixed, is exercised at both ends of its range.

package volume

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/simplyblock/atlas/statemachine"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// everyStep is the Enum marker's list, transcribed. It is written out rather
// than derived so that the assertion below compares two independent statements
// of the same set: deriving it from the graph would make the test agree with
// itself.
var everyStep = []string{"Migrating", "Validating", "Verifying"}

func TestTheStepEnumCoversEveryDeclaredState(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(graphs(0))
	want := slices.Clone(everyStep)
	slices.Sort(want)
	if diff := cmp.Diff(want, declared); diff != "" {
		t.Errorf("the graph and the Enum marker disagree (-marker +graph):\n%s", diff)
	}
}

// The CEL rule on status.step is what an Enum marker would do if a marker could
// reach a field of a type another module declares. It is a literal list in a
// struct tag, so nothing but this compares it against the graph — and it is
// compared both ways, because a rule that names a step no graph declares admits
// a status no controller can resume from.
func TestTheCELRuleCoversEveryDeclaredState(t *testing.T) {
	declared := statemachine.DeclaredMultiStates(graphs(0))
	for _, state := range declared {
		if !strings.Contains(stepCELRule, "'"+state+"'") {
			t.Errorf("status.step's CEL rule does not accept the declared step %q", state)
		}
	}
	for _, named := range celRuleValues(stepCELRule) {
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

// stepCELRule is the rule as the type declares it. Keeping a copy here is the
// cost of a rule living in a struct tag; the test above is what makes the copy
// worth having.
const stepCELRule = "!has(self.state) || self.state in ['Validating','Migrating','Verifying']"

// Every action the API accepts needs a graph, or an operation of that action
// fails at its first pass with ErrUnknownAction rather than doing anything.
func TestEveryActionDeclaresAGraph(t *testing.T) {
	declared := graphs(0)
	for _, a := range []simplyblockv1alpha2.PersistentVolumeOpsAction{
		simplyblockv1alpha2.PersistentVolumeOpsActionMigrate,
	} {
		if _, ok := declared[statemachine.Action(a)]; !ok {
			t.Errorf("action %q has no graph", a)
		}
	}
}

// TestTheMigrationIsALine. The three steps run in one order and nothing
// branches: the migration is created, the copy runs, and the paths the creation
// published are taken back.
func TestTheMigrationIsALine(t *testing.T) {
	machine, err := graphs(0).New(context.Background(), actionMigrate)
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()

	if got := machine.CurrentState(); got != stepValidating {
		t.Fatalf("the machine starts at %q, want Validating", got)
	}
	for _, next := range []step{stepMigrating, stepVerifying} {
		if err := machine.TransitionTo(context.Background(), next); err != nil {
			t.Fatalf("entering %s: %v", next, err)
		}
	}
	if !machine.IsTerminal() {
		t.Error("Verifying is not terminal, so the operation has somewhere left to go after the cleanup")
	}
}

// TestOnlyVerifyingRefusesAnAbort. Before the copy finishes there is a backend
// migration to cancel and paths to take back. After it, the volume has already
// moved: there is nothing to undo, and the operation is what finishes the work.
//
// The same graph answers for a delete, which is the point of asking it here
// rather than in two places: a deletion may never express a stop that
// spec.abort could not (design-crd-model.md §3.1).
func TestOnlyVerifyingRefusesAnAbort(t *testing.T) {
	want := []step{stepVerifying}
	if diff := cmp.Diff(want, UnabortableSteps()); diff != "" {
		t.Errorf("the steps an abort cannot be honored from (-want +got):\n%s", diff)
	}
}

// TestTheCopysDeadlineScalesWithTheSubsystemsMembers. A migration is addressed
// by the subsystem rather than by one volume, so every sibling volume in it
// moves along with the named one, and how long the copy may take is a question
// about how much there is to copy. A fixed bound would either fail a large
// subsystem that was working or wait out a small one that was stuck.
func TestTheCopysDeadlineScalesWithTheSubsystemsMembers(t *testing.T) {
	deadlineFor := func(t *testing.T, members int32) time.Duration {
		t.Helper()
		machine, err := graphs(members).New(context.Background(), actionMigrate)
		if err != nil {
			t.Fatal(err)
		}
		defer machine.Close()

		if err := machine.TransitionTo(context.Background(), stepMigrating); err != nil {
			t.Fatal(err)
		}
		remaining, bounded := machine.RequeueAfter()
		if !bounded {
			t.Fatal("the copy has no deadline at all, so a stuck migration would never be reported")
		}
		return remaining
	}

	alone := deadlineFor(t, 1)
	crowded := deadlineFor(t, 12)
	if crowded <= alone {
		t.Errorf("a 12-member subsystem gets %s and a single volume gets %s, "+
			"so the bound does not scale with what there is to copy", crowded, alone)
	}

	// A subsystem whose member count has not been read yet still gets the base
	// bound rather than none: the count arrives with the migration's creation,
	// and a step entered before it is known must still be able to time out.
	if unknown := deadlineFor(t, 0); unknown <= 0 {
		t.Error("a migration with no member count yet has no deadline")
	}
}
