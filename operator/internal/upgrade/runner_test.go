// Tests for the runner's control flow, which is where every decision the design
// makes about failure lives. A rule is a description of one thing, and what
// happens when it refuses is asserted here, once, so that adding a rule cannot
// change the shape of a stage.

package upgrade

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// recordingStep records which of its methods the runner called.
type recordingStep struct {
	namedRule

	stage Stage
	phase Phase

	done    bool
	doneErr error

	applyErr  error
	verifyErr error

	applied  int
	planned  int
	verified int
}

func (s *recordingStep) Stage() Stage { return s.stage }
func (s *recordingStep) Phase() Phase { return s.phase }

func (s *recordingStep) Plan(context.Context, *Scope) ([]Action, error) {
	s.planned++
	return []Action{{Rule: s.id, Verb: VerbUpdate, Object: ObjectRef{Name: "x"}}}, nil
}

func (s *recordingStep) Done(context.Context, *Scope) (bool, error) {
	return s.done, s.doneErr
}

func (s *recordingStep) Apply(context.Context, *Scope) error {
	s.applied++
	return s.applyErr
}

func (s *recordingStep) Verifications() []Verification {
	return []Verification{VerificationFunc{
		RuleID:  s.id + "-verified",
		Summary: "the step's post-condition",
		Fn: func(context.Context, *Scope) error {
			s.verified++
			return s.verifyErr
		},
	}}
}

func newStep(id ID, stage Stage) *recordingStep {
	return &recordingStep{namedRule: rule(id), stage: stage}
}

// testScope builds a scope over an empty fake cluster.
func testScope(t *testing.T, opts Options) *Scope {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewScope(c, "simplyblock", StageUpgrade, opts, logf.Log, DiscardReporter{})
}

func TestRunner_AppliesAStepThatIsNotDone(t *testing.T) {
	step := newStep("reparent", StageUpgrade)
	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)

	if err := NewRunner(catalog, testScope(t, Options{})).ApplyAll(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if step.applied != 1 {
		t.Fatalf("Apply called %d times, want 1", step.applied)
	}
	if step.verified != 1 {
		t.Fatalf("the post-condition was checked %d times, want 1", step.verified)
	}
}

func TestRunner_SkipsAStepThatIsAlreadyDone(t *testing.T) {
	step := newStep("reparent", StageUpgrade)
	step.done = true
	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)

	if err := NewRunner(catalog, testScope(t, Options{})).ApplyAll(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if step.applied != 0 {
		t.Fatalf("Apply was called %d times on a step that reported itself done, "+
			"which repeats a side effect on every rerun", step.applied)
	}
}

func TestRunner_VerifiesEvenAStepItSkipped(t *testing.T) {
	// A rerun that trusts Done and skips the check cannot notice that the state
	// it is resuming from is not the state it thinks it is.
	step := newStep("reparent", StageUpgrade)
	step.done = true
	step.verifyErr = errors.New("the dependents were never transferred")

	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)

	err := NewRunner(catalog, testScope(t, Options{})).ApplyAll(t.Context(), StageUpgrade)
	if err == nil {
		t.Fatal("a skipped step whose post-condition does not hold was accepted")
	}
	if !strings.Contains(err.Error(), "was not verified") {
		t.Fatalf("error = %q, want it to say the step was not verified", err)
	}
}

func TestRunner_DryRunAppliesNothing(t *testing.T) {
	step := newStep("reparent", StageUpgrade)
	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)

	scope := testScope(t, Options{DryRun: true})
	if err := NewRunner(catalog, scope).ApplyAll(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if step.applied != 0 {
		t.Fatalf("Apply was called %d times under a dry run", step.applied)
	}
	if step.planned == 0 {
		t.Fatal("Plan was never called under a dry run, so the user was shown nothing")
	}
	if step.verified != 0 {
		t.Fatal("a post-condition was checked under a dry run, and it can only fail: " +
			"nothing was applied for it to hold against")
	}
}

func TestRunner_StopsAtTheFirstFailingStep(t *testing.T) {
	first := newStep("first", StageUpgrade)
	first.applyErr = errors.New("the API server refused")
	second := newStep("second", StageUpgrade)

	catalog := NewCatalog()
	catalog.Steps.MustRegister(first, second)

	if err := NewRunner(catalog, testScope(t, Options{})).ApplyAll(t.Context(), StageUpgrade); err == nil {
		t.Fatal("a failing step did not stop the stage")
	}
	if second.applied != 0 {
		t.Fatal("a step after the one that failed was applied, which is what §25 refuses")
	}
}

func TestRunner_SkipsAStepTheCommandLineExcluded(t *testing.T) {
	step := newStep("reparent", StageUpgrade)
	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)

	scope := testScope(t, Options{Skip: []ID{"reparent"}})
	if err := NewRunner(catalog, scope).ApplyAll(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if step.applied != 0 {
		t.Fatalf("a skipped step was applied %d times", step.applied)
	}
}

func TestRunner_ReportsACheckThatCouldNotBePerformed(t *testing.T) {
	// A check that could not read the cluster has proven nothing about it,
	// which is a different outcome from a check that found nothing.
	catalog := NewCatalog()
	catalog.Checks.MustRegister(CheckFunc{
		RuleID:  "unreadable",
		Summary: "reads something the cluster would not give up",
		RunIn:   []Stage{StageUpgrade},
		Fn: func(context.Context, *Scope) (Findings, error) {
			return nil, errors.New("forbidden")
		},
	})

	_, err := NewRunner(catalog, testScope(t, Options{})).Check(t.Context(), StageUpgrade)
	if err == nil {
		t.Fatal("a check that could not run was treated as a check that passed")
	}
}

func TestRunner_CollectsFindingsWithoutDeciding(t *testing.T) {
	catalog := NewCatalog()
	catalog.Checks.MustRegister(CheckFunc{
		RuleID:  "names",
		Summary: "bounds every derived name",
		RunIn:   []Stage{StageUpgrade, StageMigrate},
		Fn: func(context.Context, *Scope) (Findings, error) {
			return Findings{{Rule: "names", Severity: SeverityError, Summary: "too long"}}, nil
		},
	})

	findings, err := NewRunner(catalog, testScope(t, Options{})).Check(t.Context(), StageUpgrade)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !findings.Blocked() {
		t.Fatal("an error finding did not block")
	}
}

func TestRunner_RunsOnlyTheChecksRegisteredForTheStage(t *testing.T) {
	ran := 0
	catalog := NewCatalog()
	catalog.Checks.MustRegister(CheckFunc{
		RuleID:  "migrate-only",
		Summary: "runs during the migration alone",
		RunIn:   []Stage{StageMigrate},
		Fn: func(context.Context, *Scope) (Findings, error) {
			ran++
			return nil, nil
		},
	})

	if _, err := NewRunner(catalog, testScope(t, Options{})).Check(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ran != 0 {
		t.Fatalf("a migrate check ran %d times during the upgrade", ran)
	}
}

func TestReadOnlyClient_RefusesEveryWrite(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	c := NewReadOnlyClient(fake.NewClientBuilder().WithScheme(scheme).Build())

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "simplyblock"}}
	for name, write := range map[string]func() error{
		"create": func() error { return c.Create(t.Context(), cm) },
		"update": func() error { return c.Update(t.Context(), cm) },
		"delete": func() error { return c.Delete(t.Context(), cm) },
		"status": func() error { return c.Status().Update(t.Context(), cm) },
	} {
		if err := write(); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("%s returned %v, want ErrReadOnly: a read-only stage promised "+
				"the user that nothing changed", name, err)
		}
	}
}
