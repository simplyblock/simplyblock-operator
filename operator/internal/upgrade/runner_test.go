// Tests for the runner's control flow, which is where every decision the design
// makes about failure lives. A rule is a description of one thing, and what
// happens when it refuses is asserted here, once, so that adding a rule cannot
// change the shape of a stage.

package upgrade

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// recordingStep records which of its methods the runner called, and over which
// subjects. It is deliberately about every subject unless told otherwise, so a
// test that cares about the fan-out says so and the rest do not have to.
type recordingStep struct {
	namedRule

	stage Stage
	phase Phase

	// about narrows what the step is responsible for, so the nil return of
	// Describe can be exercised.
	about func(Subject) bool

	// finished reports a subject already in the state the step exists to
	// produce. Describe declines those, which is what makes a rerun's plan
	// shrink, and Done claims them, which is what tells them apart from the
	// subjects the step is not about.
	finished func(Subject) bool

	doneErr     error
	validateErr error
	applyErr    error
	verifyErr   error

	described []string
	validated []string
	applied   []string
	verified  []string
}

func (s *recordingStep) Stage() Stage { return s.stage }
func (s *recordingStep) Phase() Phase { return s.phase }

func (s *recordingStep) Describe(_ context.Context, _ *Scope, subject Subject) (*Action, error) {
	if !s.responsible(subject) || s.settled(subject) {
		return nil, nil
	}
	s.described = append(s.described, subject.Ref.Name)
	return &Action{Rule: s.id, Verb: VerbUpdate, Object: subject.Ref}, nil
}

func (s *recordingStep) Done(_ context.Context, _ *Scope, subject Subject) (bool, error) {
	return s.responsible(subject) && s.settled(subject), s.doneErr
}

func (s *recordingStep) responsible(subject Subject) bool {
	return s.about == nil || s.about(subject)
}

func (s *recordingStep) settled(subject Subject) bool {
	return s.finished != nil && s.finished(subject)
}

func (s *recordingStep) Validate(_ context.Context, _ *Scope, subject Subject) error {
	s.validated = append(s.validated, subject.Ref.Name)
	return s.validateErr
}

func (s *recordingStep) Apply(_ context.Context, _ *Scope, subject Subject) error {
	s.applied = append(s.applied, subject.Ref.Name)
	return s.applyErr
}

func (s *recordingStep) Verify(_ context.Context, _ *Scope, subject Subject) error {
	s.verified = append(s.verified, subject.Ref.Name)
	return s.verifyErr
}

// newStep is a step about the upgrade alone, which is what most of the runner's
// own tests want: one subject, so the fan-out does not obscure the control flow.
func newStep(id ID, stage Stage) *recordingStep {
	return &recordingStep{
		namedRule: rule(id),
		stage:     stage,
		about:     func(subject Subject) bool { return subject.IsUpgrade() },
	}
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
	if len(step.applied) != 1 {
		t.Fatalf("Apply was called for %v, want the upgrade alone", step.applied)
	}
	if len(step.verified) != 1 {
		t.Fatalf("the post-condition was checked for %v, want the upgrade alone", step.verified)
	}
}

func TestRunner_SkipsAStepThatIsAlreadyDone(t *testing.T) {
	step := newStep("reparent", StageUpgrade)
	step.finished = func(Subject) bool { return true }
	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)

	if err := NewRunner(catalog, testScope(t, Options{})).ApplyAll(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if len(step.applied) != 0 {
		t.Fatalf("Apply was called for %v on a step that reported itself done, "+
			"which repeats a side effect on every rerun", step.applied)
	}
}

func TestRunner_ASkippedStepIsNotVerified(t *testing.T) {
	// A step that describes nothing has nothing to confirm, and the inspection
	// that would confirm it is the one Describe already made. A subject whose
	// state is not what a previous run left behind is described again and put
	// right, which TestStep_ASubjectWhoseStateRegressedIsDescribedAgain covers.
	step := newStep("reparent", StageUpgrade)
	step.finished = func(Subject) bool { return true }
	step.verifyErr = errors.New("this must not be reached")

	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)

	if err := NewRunner(catalog, testScope(t, Options{})).ApplyAll(t.Context(), StageUpgrade); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if len(step.verified) != 0 {
		t.Fatalf("verified %v on a step that described no change", step.verified)
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
	if len(step.applied) != 0 {
		t.Fatalf("Apply was called for %v under a dry run", step.applied)
	}
	if len(step.described) == 0 {
		t.Fatal("Describe was never called under a dry run, so the user was shown nothing")
	}
	if len(step.verified) != 0 {
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
	if len(second.applied) != 0 {
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
	if len(step.applied) != 0 {
		t.Fatalf("a skipped step was applied for %v", step.applied)
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
