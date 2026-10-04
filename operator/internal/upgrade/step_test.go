// Tests for the step contract, which is where the migration's idempotency
// lives. §22 states it per object: a StorageNode owned by its set is
// transferred, and one already owned by the cluster is already migrated and
// continues. A step that answered once for a whole kind would collapse three
// nodes into one all-or-nothing decision, so what is asserted here is the
// behavior at the granularity the design states it.

package upgrade

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// objectScope builds a scope whose graph holds these objects.
func objectScope(t *testing.T, objects ...client.Object) *Scope {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	scope := NewScope(c, "simplyblock", StageMigrate, Options{}, logf.Log, DiscardReporter{})
	scope.Adopt(objects...)
	return scope
}

func cm(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "simplyblock"}}
}

// objectsOnly is a step about the graph's objects and not the upgrade, which is
// the shape most of the migration's work has.
func objectsOnly() *recordingStep {
	return &recordingStep{
		namedRule: rule("reparent"),
		stage:     StageMigrate,
		phase:     PhaseOwnership,
		about:     func(subject Subject) bool { return !subject.IsUpgrade() },
	}
}

// runStep drives a step the way the runner does.
func runStep(t *testing.T, step Step, scope *Scope) error {
	t.Helper()

	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)
	return NewRunner(catalog, scope).ApplyAll(t.Context(), step.Stage())
}

// planFor reports what a step would do, the way the preflight does.
func planFor(t *testing.T, step Step, scope *Scope) []Action {
	t.Helper()

	catalog := NewCatalog()
	catalog.Steps.MustRegister(step)
	plan, err := NewRunner(catalog, scope).Plan(t.Context(), step.Stage())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan.Actions()
}

func TestStep_AppliesEverySubjectItDescribes(t *testing.T) {
	step := objectsOnly()
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	if err := runStep(t, step, scope); err != nil {
		t.Fatalf("running the step: %v", err)
	}
	if got := strings.Join(step.applied, ","); got != "a,b,c" {
		t.Fatalf("applied %q, want every subject in graph order", got)
	}
	if got := strings.Join(step.verified, ","); got != "a,b,c" {
		t.Fatalf("verified %q, want every subject it changed", got)
	}
}

func TestStep_DeclinesASubjectItIsNotAbout(t *testing.T) {
	// A nil from Describe is no operation, and the subject must not be
	// validated, applied, or verified on the strength of it.
	step := objectsOnly()
	step.about = func(subject Subject) bool { return subject.Ref.Name == "b" }
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	if err := runStep(t, step, scope); err != nil {
		t.Fatalf("running the step: %v", err)
	}
	if got := strings.Join(step.applied, ","); got != "b" {
		t.Fatalf("applied %q, want only the subject the step is about", got)
	}
	if len(step.verified) != 1 {
		t.Fatalf("verified %v, want only that subject", step.verified)
	}
}

func TestStep_DescribesNothingForASubjectAlreadyInTheTargetState(t *testing.T) {
	// A StorageNode already owned by its StorageCluster is no operation. It is
	// not a change to skip, it is a change that does not exist.
	step := objectsOnly()
	step.finished = func(Subject) bool { return true }
	scope := objectScope(t, cm("a"), cm("b"))

	if err := runStep(t, step, scope); err != nil {
		t.Fatalf("running the step: %v", err)
	}
	for name, got := range map[string][]string{
		"described": step.described,
		"validated": step.validated,
		"applied":   step.applied,
		"verified":  step.verified,
	} {
		if len(got) != 0 {
			t.Errorf("%s %v on a subject that needed no change", name, got)
		}
	}
}

func TestStep_ResumesOnTheSubjectsThatAreNotFinished(t *testing.T) {
	// The behavior a step-scoped decision cannot express. Two of three were
	// reparented before the run was killed, and the third is the only one left.
	step := objectsOnly()
	step.finished = func(subject Subject) bool { return subject.Ref.Name != "c" }
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	if err := runStep(t, step, scope); err != nil {
		t.Fatalf("running the step: %v", err)
	}
	if got := strings.Join(step.applied, ","); got != "c" {
		t.Fatalf("applied %q, want the one subject that was not already finished", got)
	}
}

func TestStep_ASubjectWhoseStateRegressedIsDescribedAgain(t *testing.T) {
	// What makes a rerun self-healing. Describe inspects the subject, so a
	// subject a previous run left in the wrong state describes an action again
	// and is put right, rather than being reported as a change that does not
	// hold.
	step := objectsOnly()
	step.finished = func(subject Subject) bool { return subject.Ref.Name == "a" }
	scope := objectScope(t, cm("a"), cm("b"))

	if err := runStep(t, step, scope); err != nil {
		t.Fatalf("running the step: %v", err)
	}
	if got := strings.Join(step.applied, ","); got != "b" {
		t.Fatalf("applied %q, want the subject whose state was not what it should be", got)
	}
}

func TestStep_APreconditionFailureStopsBeforeTheNextSubject(t *testing.T) {
	step := objectsOnly()
	step.validateErr = errors.New("its cluster does not exist")
	scope := objectScope(t, cm("a"), cm("b"))

	err := runStep(t, step, scope)
	if err == nil {
		t.Fatal("a failed precondition did not stop the step")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error = %q, want it to say the subject was not ready", err)
	}
	if len(step.applied) != 0 {
		t.Fatalf("applied %v after a precondition failed", step.applied)
	}
	if len(step.validated) != 1 {
		t.Fatalf("validated %v, want it to stop at the first failure", step.validated)
	}
}

func TestStep_PlanReportsOnlyTheOutstandingWork(t *testing.T) {
	// A plan taken after a partial run shows what is left, not what was
	// originally intended, so a user rerunning sees the run shrink.
	step := objectsOnly()
	step.finished = func(subject Subject) bool { return subject.Ref.Name != "c" }
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	actions := planFor(t, step, scope)
	if len(actions) != 1 {
		t.Fatalf("planned %d actions, want the 1 that is outstanding:\n%v", len(actions), actions)
	}
	if actions[0].Object.Name != "c" {
		t.Fatalf("planned %q, want the subject that is not finished", actions[0].Object.Name)
	}
}

func TestStep_PlanChangesNothing(t *testing.T) {
	step := objectsOnly()
	scope := objectScope(t, cm("a"), cm("b"))

	planFor(t, step, scope)
	if len(step.applied) != 0 || len(step.verified) != 0 {
		t.Fatalf("planning applied %v and verified %v", step.applied, step.verified)
	}
}

func TestStep_TheWalkIsDeterministic(t *testing.T) {
	// The plan a user reads and the sequence the migration performs have to be
	// the same sequence twice, and the graph indexes its kinds in a map.
	objects := []client.Object{
		cm("c"), cm("a"), cm("b"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "simplyblock"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "simplyblock"}},
	}

	var first string
	for run := range 20 {
		step := objectsOnly()
		if err := runStep(t, step, objectScope(t, objects...)); err != nil {
			t.Fatalf("running the step: %v", err)
		}
		order := strings.Join(step.applied, ",")
		if run == 0 {
			first = order
			continue
		}
		if order != first {
			t.Fatalf("two runs applied in different orders:\n%s\n%s", first, order)
		}
	}
}

func TestCovered_TellsFinishedFromUntouched(t *testing.T) {
	// The two halves of a nil Describe. A subject no step describes is either
	// finished or one nothing has taken responsibility for, and those are very
	// different answers to whether the migration is complete.
	step := objectsOnly()
	step.finished = func(subject Subject) bool { return subject.Ref.Name == "a" }
	scope := objectScope(t, cm("a"), cm("b"))

	covered, err := Covered(t.Context(), scope, step)
	if err != nil {
		t.Fatalf("Covered: %v", err)
	}
	if len(covered.Outstanding) != 1 || covered.Outstanding[0].Ref.Name != "b" {
		t.Errorf("outstanding = %v, want the subject still to change", covered.Outstanding)
	}
	if len(covered.Finished) != 1 || covered.Finished[0].Ref.Name != "a" {
		t.Errorf("finished = %v, want the subject already in the target state", covered.Finished)
	}
	// The upgrade itself: this step is not about it, and it claims nothing.
	if len(covered.Untouched) != 1 || !covered.Untouched[0].IsUpgrade() {
		t.Errorf("untouched = %v, want the upgrade, which this step does not claim",
			covered.Untouched)
	}
}

func TestSubjects_TheUpgradeIsOneOfThem(t *testing.T) {
	// §9.1's steps act on the upgrade rather than on anything in the cluster,
	// so it is a subject like any other and comes first.
	subjects := objectScope(t, cm("a")).Subjects()

	if len(subjects) != 2 {
		t.Fatalf("got %d subjects, want the upgrade and the one object", len(subjects))
	}
	if !subjects[0].IsUpgrade() {
		t.Fatalf("the first subject is %s, want the upgrade", subjects[0])
	}
	if subjects[0].Ref.String() != "Upgrade simplyblock/upgrade" {
		t.Fatalf("the upgrade names itself %q", subjects[0].Ref)
	}
	if subjects[1].IsUpgrade() || subjects[1].Object == nil {
		t.Fatal("the object subject does not carry its object")
	}
}
