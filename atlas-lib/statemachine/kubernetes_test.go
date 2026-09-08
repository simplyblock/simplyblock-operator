// Tests for the Kubernetes forms of Snapshot: the round trip a controller
// performs on every reconcile, and the two asymmetries in it that a naive
// conversion gets wrong, which are an absent deadline and a state the graph does
// not declare.

package statemachine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/statemachine"
)

type kubeStep string

const (
	stepValidating kubeStep = "Validating"
	stepSuspending kubeStep = "Suspending"
	stepRemoving   kubeStep = "Removing"
	stepPreparing  kubeStep = "Preparing"
	stepPromoting  kubeStep = "Promoting"
)

func kubeConfig() statemachine.Config[kubeStep] {
	return statemachine.Config[kubeStep]{
		Initial: stepValidating,
		States: map[kubeStep]statemachine.StateDef[kubeStep]{
			stepValidating: {To: []kubeStep{stepSuspending}},
			stepSuspending: {To: []kubeStep{stepRemoving}},
			stepRemoving:   {},
		},
	}
}

func TestToKube_CarriesStateAndDeadline(t *testing.T) {
	deadline := time.Date(2026, 8, 28, 11, 40, 0, 0, time.UTC)
	kube := statemachine.ToKube(statemachine.Snapshot[kubeStep]{
		State:    stepSuspending,
		Deadline: deadline,
	})

	if kube.State != "Suspending" {
		t.Fatalf("State = %q, want %q", kube.State, "Suspending")
	}
	if kube.Deadline == nil {
		t.Fatal("Deadline is nil, want the instant the snapshot carried")
	}
	if !kube.Deadline.Time.Equal(deadline) {
		t.Fatalf("Deadline = %v, want %v", kube.Deadline.Time, deadline)
	}
}

func TestToKube_NoDeadlineIsAbsentNotZero(t *testing.T) {
	kube := statemachine.ToKube(statemachine.Snapshot[kubeStep]{State: stepRemoving})

	if kube.Deadline != nil {
		t.Fatalf("Deadline = %v, want nil: an unbounded state must not serialize "+
			"as an instant in 1970", kube.Deadline)
	}
}

func TestFromKube_TypesTheState(t *testing.T) {
	snap := statemachine.FromKube[kubeStep](statemachine.KubeSnapshot{State: "Suspending"})

	if snap.State != stepSuspending {
		t.Fatalf("State = %q, want %q", snap.State, stepSuspending)
	}
}

func TestFromKube_AbsentDeadlineIsZero(t *testing.T) {
	snap := statemachine.FromKube[kubeStep](statemachine.KubeSnapshot{State: "Removing"})

	if !snap.Deadline.IsZero() {
		t.Fatalf("Deadline = %v, want the zero time, which is a state that never expires",
			snap.Deadline)
	}
}

func TestKubeSnapshot_RoundTripsThroughTheMachine(t *testing.T) {
	ctx := context.Background()

	first, err := statemachine.New(ctx, kubeConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := first.TransitionTo(ctx, stepSuspending); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	stored := statemachine.ToKube(first.Snapshot())
	first.Close()

	// A later process, holding only what the resource stored.
	second, err := statemachine.NewFromSnapshot(ctx, kubeConfig(),
		statemachine.FromKube[kubeStep](stored))
	if err != nil {
		t.Fatalf("NewFromSnapshot: %v", err)
	}
	defer second.Close()

	if got := second.CurrentState(); got != stepSuspending {
		t.Fatalf("CurrentState = %q, want %q", got, stepSuspending)
	}
	if err := second.TransitionTo(ctx, stepRemoving); err != nil {
		t.Fatalf("the restored machine rejected a legal edge: %v", err)
	}
}

func TestKubeSnapshot_EmptyStateRestoresToInitial(t *testing.T) {
	ctx := context.Background()

	sm, err := statemachine.NewFromSnapshot(ctx, kubeConfig(),
		statemachine.FromKube[kubeStep](statemachine.KubeSnapshot{}))
	if err != nil {
		t.Fatalf("NewFromSnapshot on an unreconciled resource: %v", err)
	}
	defer sm.Close()

	if got := sm.CurrentState(); got != stepValidating {
		t.Fatalf("CurrentState = %q, want the graph's initial state %q", got, stepValidating)
	}
}

func TestKubeSnapshot_UndeclaredStateIsRejectedByRestoreNotByFromKube(t *testing.T) {
	ctx := context.Background()

	// FromKube types the string and does not validate it: the graph is not known
	// here, and a state belonging to a different action is legitimate input.
	snap := statemachine.FromKube[kubeStep](statemachine.KubeSnapshot{State: "Promoting"})
	if snap.State != kubeStep("Promoting") {
		t.Fatalf("State = %q, want the string typed through unchanged", snap.State)
	}

	_, err := statemachine.NewFromSnapshot(ctx, kubeConfig(), snap)
	if !errors.Is(err, statemachine.ErrUnknownState) {
		t.Fatalf("NewFromSnapshot error = %v, want ErrUnknownState", err)
	}
}

func TestKubeSnapshot_ExpiredDeadlineRestoresExpired(t *testing.T) {
	ctx := context.Background()
	past := metav1.NewTime(time.Now().Add(-time.Hour))

	sm, err := statemachine.NewFromSnapshot(ctx, kubeConfig(),
		statemachine.FromKube[kubeStep](statemachine.KubeSnapshot{
			State:    "Suspending",
			Deadline: &past,
		}))
	if err != nil {
		t.Fatalf("NewFromSnapshot: %v", err)
	}
	defer sm.Close()

	if !sm.TimeoutReached() {
		t.Fatal("TimeoutReached is false: a deadline that passed while the controller " +
			"was down has to restore as already expired")
	}
}

func TestKubeSnapshot_DeepCopyDoesNotShareTheDeadline(t *testing.T) {
	when := metav1.NewTime(time.Date(2026, 8, 28, 11, 40, 0, 0, time.UTC))
	original := statemachine.KubeSnapshot{State: "Suspending", Deadline: &when}

	clone := original.DeepCopy()
	if clone.Deadline == original.Deadline {
		t.Fatal("DeepCopy shares the deadline pointer with the original")
	}
	if !clone.Deadline.Time.Equal(original.Deadline.Time) {
		t.Fatalf("DeepCopy deadline = %v, want %v", clone.Deadline.Time, original.Deadline.Time)
	}

	var nilSnapshot *statemachine.KubeSnapshot
	if nilSnapshot.DeepCopy() != nil {
		t.Fatal("DeepCopy of a nil receiver is not nil")
	}
}

func TestKubeDeadline_ReportsWhetherOneIsSet(t *testing.T) {
	if _, ok := (statemachine.KubeSnapshot{State: "Removing"}).KubeDeadline(); ok {
		t.Fatal("KubeDeadline reports a deadline on a snapshot that has none")
	}

	when := metav1.NewTime(time.Date(2026, 8, 28, 11, 40, 0, 0, time.UTC))
	got, ok := (statemachine.KubeSnapshot{State: "Suspending", Deadline: &when}).KubeDeadline()
	if !ok {
		t.Fatal("KubeDeadline reports no deadline on a snapshot that has one")
	}
	if !got.Equal(when.Time) {
		t.Fatalf("KubeDeadline = %v, want %v", got, when.Time)
	}
}

// A stored state the graph does not declare is the failure a shared KubeSnapshot
// makes more likely, because the step values now live in three places that have
// to agree: the graph, the Enum marker on the kind's step type, and the CEL rule
// at the use site. The rejection therefore has to say what was expected, not just
// that something was wrong.
func TestRestore_UnknownStateNamesTheDeclaredStates(t *testing.T) {
	ctx := context.Background()

	_, err := statemachine.NewFromSnapshot(ctx, kubeConfig(),
		statemachine.FromKube[kubeStep](statemachine.KubeSnapshot{State: "Promoting"}))
	if !errors.Is(err, statemachine.ErrUnknownState) {
		t.Fatalf("error = %v, want ErrUnknownState", err)
	}

	message := err.Error()
	if !strings.Contains(message, "Promoting") {
		t.Fatalf("error %q does not name the state that was rejected", message)
	}
	for _, declared := range []string{"Validating", "Suspending", "Removing"} {
		if !strings.Contains(message, declared) {
			t.Fatalf("error %q does not name the declared state %q, so a reader "+
				"cannot tell a stale value from one belonging to another action",
				message, declared)
		}
	}
}

// The three lists that have to agree are checkable, which is the point of
// exposing what a graph declares.
func TestDeclaredStates_IsTheGraphsOwnList(t *testing.T) {
	got := statemachine.DeclaredStates(kubeConfig())
	want := []string{"Removing", "Suspending", "Validating"}

	if len(got) != len(want) {
		t.Fatalf("DeclaredStates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DeclaredStates = %v, want %v (sorted, so a test can compare it)", got, want)
		}
	}
}

func TestDeclaredMultiStates_IsTheUnionAcrossActions(t *testing.T) {
	graphs := statemachine.MultiConfig[kubeStep]{
		"Remove": kubeConfig(),
		"Migrate": {
			Initial: stepPreparing,
			States: map[kubeStep]statemachine.StateDef[kubeStep]{
				stepPreparing: {To: []kubeStep{stepPromoting}},
				stepPromoting: {},
			},
		},
	}

	got := statemachine.DeclaredMultiStates(graphs)
	want := []string{"Preparing", "Promoting", "Removing", "Suspending", "Validating"}

	if len(got) != len(want) {
		t.Fatalf("DeclaredStates = %v, want the union %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DeclaredStates = %v, want %v", got, want)
		}
	}
}
