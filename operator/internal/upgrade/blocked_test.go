// Tests for a step that describes its work and cannot yet perform it, which is
// how the plan stays complete while the migration is being built.
//
// The property that matters is the refusal. A stage that performed its
// implemented steps and stopped at the first blocked one would leave a cluster
// halfway through an upgrade nothing can finish, and it would do so having
// reported the earlier steps as successes.

package upgrade

import (
	"context"
	"strings"
	"testing"
)

// describedOnly is a step that reports work and refuses to do it.
type describedOnly struct {
	namedRule

	reason  string
	applied int
}

func (d *describedOnly) Stage() Stage      { return StageUpgrade }
func (d *describedOnly) Phase() Phase      { return "" }
func (d *describedOnly) BlockedBy() string { return d.reason }

func (d *describedOnly) Describe(_ context.Context, _ *Scope, subject Subject) (*Action, error) {
	if !subject.IsUpgrade() {
		return nil, nil
	}
	return &Action{Rule: d.id, Verb: VerbCreate, Object: subject.Ref, Detail: "something"}, nil
}

func (d *describedOnly) Done(context.Context, *Scope, Subject) (bool, error) { return false, nil }
func (d *describedOnly) Validate(context.Context, *Scope, Subject) error     { return nil }
func (d *describedOnly) Verify(context.Context, *Scope, Subject) error       { return nil }

func (d *describedOnly) Apply(context.Context, *Scope, Subject) error {
	d.applied++
	return nil
}

func blockedStep(id ID) *describedOnly {
	return &describedOnly{namedRule: rule(id), reason: "the thing it needs does not exist yet"}
}

func TestBlocked_ThePlanCarriesTheWorkAndMarksIt(t *testing.T) {
	// A step that was simply absent would leave the plan short of what the
	// upgrade owes, and silence reads as nothing to do.
	catalog := NewCatalog()
	catalog.Steps.MustRegister(blockedStep("deploy-webhook"))

	plan, err := NewRunner(catalog, testScope(t, Options{})).Plan(t.Context(), StageUpgrade)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("planned %d tasks, want the described one", len(plan.Tasks))
	}

	// The mark belongs to the task rather than to each subject: it is a fact
	// about the step, and repeating it on every object it would touch would
	// say it ninety-five times for the release handover.
	if plan.Tasks[0].Blocked == "" {
		t.Error("the task is not marked, so a reader would take it for work this build can do")
	}
	if got := plan.Unimplemented(); len(got) != 1 {
		t.Errorf("Unimplemented reports %d tasks, want 1", len(got))
	}

	// A step acting on the upgrade has one subject that carries no
	// information, so the plan prints the task alone.
	if !plan.Tasks[0].Collapsed() {
		t.Error("a task whose only subject is the upgrade was not collapsed, so the " +
			"plan would print the step and then repeat it")
	}
}

func TestBlocked_TheSummarySaysTheStageCannotRun(t *testing.T) {
	catalog := NewCatalog()
	catalog.Steps.MustRegister(blockedStep("deploy-webhook"), blockedStep("apply-crds"))

	plan, err := NewRunner(catalog, testScope(t, Options{})).Plan(t.Context(), StageUpgrade)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	summary := strings.Join(plan.Summary(), "\n")
	for _, want := range []string{"2 tasks in total", "cannot be run yet"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, summary)
		}
	}
}

func TestBlocked_TheStageRefusesBeforeApplyingAnything(t *testing.T) {
	// The property this exists for. The implemented step must not run, because
	// a stage that cannot be completed is one that must not be started.
	implemented := newStep("does-work", StageUpgrade)
	catalog := NewCatalog()
	catalog.Steps.MustRegister(implemented, blockedStep("deploy-webhook"))

	err := NewRunner(catalog, testScope(t, Options{})).ApplyAll(t.Context(), StageUpgrade)
	if err == nil {
		t.Fatal("a stage with a blocked step in it was started")
	}
	if !strings.Contains(err.Error(), "must not start") {
		t.Errorf("error = %q, want it to say why", err)
	}
	if !strings.Contains(err.Error(), "deploy-webhook") {
		t.Errorf("error = %q, want it to name the step that is missing", err)
	}
	if len(implemented.applied) != 0 {
		t.Fatalf("the implemented step ran for %v before the refusal, leaving the "+
			"cluster partway through", implemented.applied)
	}
}

func TestBlocked_SkippingTheBlockedStepLetsTheRestRun(t *testing.T) {
	// The escape hatch is the one that already exists. An operator who knows
	// what is missing can exclude it by name, and the exclusion is reported.
	implemented := newStep("does-work", StageUpgrade)
	catalog := NewCatalog()
	catalog.Steps.MustRegister(implemented, blockedStep("deploy-webhook"))

	scope := testScope(t, Options{Skip: []ID{"deploy-webhook"}})
	if err := NewRunner(catalog, scope).ApplyAll(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if len(implemented.applied) != 1 {
		t.Fatalf("the implemented step ran for %v, want the upgrade once", implemented.applied)
	}
}

func TestBlocked_AStageWithNoBlockedStepsRunsNormally(t *testing.T) {
	implemented := newStep("does-work", StageUpgrade)
	catalog := NewCatalog()
	catalog.Steps.MustRegister(implemented)

	if err := NewRunner(catalog, testScope(t, Options{})).ApplyAll(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if len(implemented.applied) != 1 {
		t.Fatalf("applied for %v, want the upgrade once", implemented.applied)
	}
}
