// Tests for the per-object adapter, which holds the contract every per-object
// step would otherwise restate. What is asserted is the behavior a step-scoped
// Done cannot express: a run killed after the second of three objects resumes
// on the third alone, and the plan it reports shrinks to match.

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// reparenter is a per-object step over ConfigMaps. It describes a change for
// the ones it is about, treats a marked object as done, and records what it
// touched.
type reparenter struct {
	namedRule

	// about narrows what the step is responsible for, so the nil return of
	// Describe can be exercised.
	about func(client.Object) bool

	// marked reports an object whose change is already present.
	marked func(client.Object) bool

	validateErr error
	applyErr    error
	verifyErr   error

	described []string
	validated []string
	applied   []string
	verified  []string
}

func (r *reparenter) Stage() Stage { return StageMigrate }
func (r *reparenter) Phase() Phase { return PhaseOwnership }

func (r *reparenter) Describe(_ context.Context, s *Scope, obj client.Object) (*Action, error) {
	if r.about != nil && !r.about(obj) {
		return nil, nil
	}
	r.described = append(r.described, obj.GetName())
	return &Action{
		Rule:   r.id,
		Verb:   VerbReparent,
		Object: s.Ref(obj),
		Detail: "owner: StorageNodeSet/set-a → StorageCluster/cluster-a",
	}, nil
}

func (r *reparenter) Done(_ context.Context, _ *Scope, obj client.Object) (bool, error) {
	return r.marked != nil && r.marked(obj), nil
}

func (r *reparenter) Validate(_ context.Context, _ *Scope, obj client.Object) error {
	r.validated = append(r.validated, obj.GetName())
	return r.validateErr
}

func (r *reparenter) Apply(_ context.Context, _ *Scope, obj client.Object) error {
	r.applied = append(r.applied, obj.GetName())
	return r.applyErr
}

func (r *reparenter) Verify(_ context.Context, _ *Scope, obj client.Object) error {
	r.verified = append(r.verified, obj.GetName())
	return r.verifyErr
}

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

func TestPerObject_AppliesEverySubject(t *testing.T) {
	step := &reparenter{namedRule: rule("reparent")}
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	if err := PerObject(step).Apply(t.Context(), scope); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := strings.Join(step.applied, ","); got != "a,b,c" {
		t.Fatalf("applied %q, want every subject in graph order", got)
	}
	if got := strings.Join(step.verified, ","); got != "a,b,c" {
		t.Fatalf("verified %q, want every subject", got)
	}
}

func TestPerObject_SkipsAnObjectTheStepIsNotAbout(t *testing.T) {
	// A nil from Describe is an explicit declination, and the object must not
	// be validated, applied, or verified on the strength of it.
	step := &reparenter{
		namedRule: rule("reparent"),
		about:     func(obj client.Object) bool { return obj.GetName() == "b" },
	}
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	if err := PerObject(step).Apply(t.Context(), scope); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := strings.Join(step.applied, ","); got != "b" {
		t.Fatalf("applied %q, want only the subject the step is about", got)
	}
	if len(step.verified) != 1 {
		t.Fatalf("verified %v, want only the subject", step.verified)
	}
}

func TestPerObject_ResumesOnTheObjectsThatAreNotDone(t *testing.T) {
	// The behavior a step-scoped Done cannot express. Two of three were
	// reparented before the run was killed, and the third is the only one left.
	step := &reparenter{
		namedRule: rule("reparent"),
		marked:    func(obj client.Object) bool { return obj.GetName() != "c" },
	}
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	if err := PerObject(step).Apply(t.Context(), scope); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := strings.Join(step.applied, ","); got != "c" {
		t.Fatalf("applied %q, want the one object that was not already done", got)
	}
}

func TestPerObject_VerifiesAnObjectThatWasAlreadyDone(t *testing.T) {
	// A rerun that trusts Done and skips the check cannot notice that the state
	// it resumed from is not the state it thinks.
	step := &reparenter{
		namedRule: rule("reparent"),
		marked:    func(client.Object) bool { return true },
		verifyErr: errors.New("the owner reference is not what it should be"),
	}
	scope := objectScope(t, cm("a"))

	err := PerObject(step).Apply(t.Context(), scope)
	if err == nil {
		t.Fatal("an object reported done whose change does not hold was accepted")
	}
	if len(step.applied) != 0 {
		t.Fatal("the step reapplied a change it had reported as already present")
	}
}

func TestPerObject_DoesNotValidateAnObjectThatWasAlreadyDone(t *testing.T) {
	// The precondition is for the change that is about to be made, and nothing
	// is about to be made here.
	step := &reparenter{
		namedRule:   rule("reparent"),
		marked:      func(client.Object) bool { return true },
		validateErr: errors.New("the precondition no longer holds"),
	}

	if err := PerObject(step).Apply(t.Context(), objectScope(t, cm("a"))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(step.validated) != 0 {
		t.Fatalf("validated %v on an object that needed no change", step.validated)
	}
}

func TestPerObject_APreconditionFailureStopsBeforeTheNextObject(t *testing.T) {
	step := &reparenter{
		namedRule:   rule("reparent"),
		validateErr: errors.New("its cluster does not exist"),
	}
	scope := objectScope(t, cm("a"), cm("b"))

	err := PerObject(step).Apply(t.Context(), scope)
	if err == nil {
		t.Fatal("a failed precondition did not stop the step")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error = %q, want it to say the object was not ready", err)
	}
	if len(step.applied) != 0 {
		t.Fatalf("applied %v after a precondition failed", step.applied)
	}
	if len(step.validated) != 1 {
		t.Fatalf("validated %v, want it to stop at the first failure", step.validated)
	}
}

func TestPerObject_PlanReportsOnlyTheOutstandingWork(t *testing.T) {
	// A plan taken after a partial run shows what is left, not what was
	// originally intended, so a user rerunning sees the run shrink.
	step := &reparenter{
		namedRule: rule("reparent"),
		marked:    func(obj client.Object) bool { return obj.GetName() != "c" },
	}
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	actions, err := PerObject(step).Plan(t.Context(), scope)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("planned %d actions, want the 1 that is outstanding:\n%v", len(actions), actions)
	}
	if actions[0].Object.Name != "c" {
		t.Fatalf("planned %q, want the object that is not done", actions[0].Object.Name)
	}
}

func TestPerObject_PlanChangesNothing(t *testing.T) {
	step := &reparenter{namedRule: rule("reparent")}
	scope := objectScope(t, cm("a"), cm("b"))

	if _, err := PerObject(step).Plan(t.Context(), scope); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(step.applied) != 0 || len(step.verified) != 0 {
		t.Fatalf("planning applied %v and verified %v", step.applied, step.verified)
	}
}

func TestPerObject_DoneWhenEverySubjectIs(t *testing.T) {
	step := &reparenter{
		namedRule: rule("reparent"),
		marked:    func(client.Object) bool { return true },
	}

	done, err := PerObject(step).Done(t.Context(), objectScope(t, cm("a"), cm("b")))
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	if !done {
		t.Fatal("a step whose every subject is done reported itself outstanding")
	}
}

func TestPerObject_NotDoneWhenOneSubjectIsNot(t *testing.T) {
	step := &reparenter{
		namedRule: rule("reparent"),
		marked:    func(obj client.Object) bool { return obj.GetName() == "a" },
	}

	done, err := PerObject(step).Done(t.Context(), objectScope(t, cm("a"), cm("b")))
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	if done {
		t.Fatal("a step with one outstanding subject reported itself done")
	}
}

func TestPerObject_AStepWithNoSubjectsIsDone(t *testing.T) {
	// A migration whose StorageNodeSets are already retired has nothing for the
	// reparenting to do, and reporting it outstanding would leave the phase
	// applying a step that walks an empty set.
	step := &reparenter{
		namedRule: rule("reparent"),
		about:     func(client.Object) bool { return false },
	}

	done, err := PerObject(step).Done(t.Context(), objectScope(t, cm("a")))
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	if !done {
		t.Fatal("a step with no subjects reported itself outstanding")
	}
}

func TestPerObject_TheWalkIsDeterministic(t *testing.T) {
	// The plan a user reads and the sequence the migration performs have to be
	// the same sequence twice, and the graph indexes its kinds in a map.
	objects := []client.Object{
		cm("c"), cm("a"), cm("b"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "simplyblock"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "simplyblock"}},
	}

	var first string
	for run := range 20 {
		step := &reparenter{namedRule: rule("reparent")}
		if err := PerObject(step).Apply(t.Context(), objectScope(t, objects...)); err != nil {
			t.Fatalf("Apply: %v", err)
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

func TestSubjects_ReportsWhatAStepIsResponsibleFor(t *testing.T) {
	// It is what a coverage check measures against: an object no step describes
	// is one nothing has taken responsibility for.
	step := &reparenter{
		namedRule: rule("reparent"),
		about:     func(obj client.Object) bool { return obj.GetName() != "b" },
	}
	scope := objectScope(t, cm("a"), cm("b"), cm("c"))

	subjects, err := Subjects(t.Context(), scope, step)
	if err != nil {
		t.Fatalf("Subjects: %v", err)
	}
	if len(subjects) != 2 {
		t.Fatalf("got %d subjects, want the 2 the step is about:\n%v", len(subjects), subjects)
	}
}

func TestPerObject_RunsThroughTheRunnerLikeAnyOtherStep(t *testing.T) {
	// The point of the adapter: the phase graph, the registry, and the runner
	// are unchanged.
	step := &reparenter{namedRule: rule("reparent")}
	catalog := NewCatalog()
	catalog.Steps.MustRegister(PerObject(step))

	scope := objectScope(t, cm("a"), cm("b"))
	if err := NewRunner(catalog, scope).ApplyAll(t.Context(), StageMigrate); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if got := strings.Join(step.applied, ","); got != "a,b" {
		t.Fatalf("applied %q through the runner", got)
	}
}
