package volumemigration

import (
	"context"
	"testing"
	"time"

	"github.com/simplyblock/atlas/statemachine"
	"github.com/simplyblock/atlas/statemachine/smtest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/ctrltest"
)

// graphMachine builds the lifecycle machine for assertions about the graph itself.
// No hook runs on these paths, so the reconciler needs nothing wired up.
func graphMachine(t *testing.T) *statemachine.Machine[phase] {
	t.Helper()
	r := &Reconciler{}
	sm := r.machineFor(t.Context(), &migrationPass{vm: baseVM()})
	t.Cleanup(sm.Close)
	return sm
}

// The lifecycle is what the graph says it is, and nothing in it is stranded: a phase
// no edge points at would be a phase the controller can never reach, which no test
// asserting on behaviour would notice.
func TestMigrationGraph_ShapeAndReachability(t *testing.T) {
	sm := graphMachine(t)
	c := smtest.Check(t, sm)

	c.In(phasePending).Reachable().NoDeadline()

	for _, tc := range []struct {
		from  phase
		to    []phase
		final bool
	}{
		// Pending can be re-entered (a deferral) and aborted (a user who has changed
		// their mind should not have to wait out the retry window first).
		{from: phasePending, to: []phase{phasePending, phaseValidating, phaseFailed, phaseAborted}},
		{from: phaseValidating, to: []phase{phaseRunning, phaseFailed, phaseAborted}},
		{from: phaseRunning, to: []phase{phaseCompleted, phaseFailed, phaseAborted}},
		{from: phaseCompleted, final: true},
		{from: phaseFailed, final: true},
		{from: phaseAborted, final: true},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			sm := graphMachine(t)
			if err := sm.Restore(statemachine.Snapshot[phase]{State: tc.from}); err != nil {
				t.Fatalf("restore %q: %v", tc.from, err)
			}
			c := smtest.Check(t, sm).In(tc.from).Allows(tc.to...)
			if tc.final {
				c.Terminal()
			} else {
				c.NotTerminal()
			}
		})
	}
}

// Neither Validating nor Running may be re-entered. Validating's hook submits the
// migration and Running's continues it, so a self-edge on either would mean a second
// migration for the same subsystem or a ContinueMigration the control plane rejects.
func TestMigrationGraph_NoSelfEdgeOnTheWorkingPhases(t *testing.T) {
	for _, p := range []phase{phaseValidating, phaseRunning} {
		t.Run(string(p), func(t *testing.T) {
			sm := graphMachine(t)
			if err := sm.Restore(statemachine.Snapshot[phase]{State: p}); err != nil {
				t.Fatalf("restore %q: %v", p, err)
			}
			smtest.Check(t, sm).Refuses(t.Context(), p)
		})
	}
}

// Skipping a phase is refused by the graph rather than by whoever asked: a migration
// cannot reach Running without having been validated, or Completed without having run.
func TestMigrationGraph_RefusesSkippedPhases(t *testing.T) {
	for _, tc := range []struct{ from, to phase }{
		{phasePending, phaseRunning},
		{phasePending, phaseCompleted},
		{phaseValidating, phaseCompleted},
	} {
		t.Run(string(tc.from)+"_to_"+string(tc.to), func(t *testing.T) {
			sm := graphMachine(t)
			if err := sm.Restore(statemachine.Snapshot[phase]{State: tc.from}); err != nil {
				t.Fatalf("restore %q: %v", tc.from, err)
			}
			smtest.Check(t, sm).Refuses(t.Context(), tc.to)
		})
	}
}

// The Validating bound is what maxConsumerWait is spent out of: the phase waits for
// consumers only while a full Job deadline still fits in what is left, so the two
// constants have to compose or the wait silently changes length.
func TestPhaseBound(t *testing.T) {
	if got := phaseBound(phaseValidating); got != maxConsumerWait+validationJobDeadline {
		t.Errorf("Validating bound = %v, want maxConsumerWait + validationJobDeadline", got)
	}
	if got := phaseBound(phasePending); got != maxMigrationDeferral {
		t.Errorf("Pending bound = %v, want %v", got, maxMigrationDeferral)
	}
	// Running is bounded by how much data there is, not by the clock.
	for _, p := range []phase{phaseRunning, phaseCompleted, phaseFailed, phaseAborted} {
		if got := phaseBound(p); got != 0 {
			t.Errorf("%s bound = %v, want none", p, got)
		}
	}
}

// mayWaitForConsumers is the consumer wait stated in terms of the phase bound; it has
// to come out at maxConsumerWait after the phase was entered.
func TestMayWaitForConsumers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		elapsed  time.Duration
		wantWait bool
	}{
		{"just entered", 0, true},
		{"almost at the limit", maxConsumerWait - 2*time.Second, true},
		{"past the limit", maxConsumerWait + time.Second, false},
		{"bound exhausted", phaseBound(phaseValidating) + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{}
			vm := validatingVM("")
			deadline := metav1.NewTime(time.Now().Add(phaseBound(phaseValidating) - tc.elapsed))
			vm.Status.PhaseDeadline = &deadline

			p := &migrationPass{vm: vm}
			sm := r.machineFor(t.Context(), p)
			defer sm.Close()
			if err := r.restorePhase(t.Context(), sm, vm); err != nil {
				t.Fatalf("restorePhase: %v", err)
			}
			if got := mayWaitForConsumers(sm); got != tc.wantWait {
				t.Errorf("mayWaitForConsumers = %v, want %v after %v in Validating", got, tc.wantWait, tc.elapsed)
			}
		})
	}
}

// A phase with no deadline in status is an object written before status.phaseDeadline
// existed. It gets its full bound rather than being left unbounded, because a
// migration that can wait forever is the failure the bound exists to prevent.
func TestRestorePhase(t *testing.T) {
	r := &Reconciler{}

	t.Run("no phase starts at the beginning", func(t *testing.T) {
		vm := baseVM()
		p := &migrationPass{vm: vm}
		sm := r.machineFor(t.Context(), p)
		defer sm.Close()
		if err := r.restorePhase(t.Context(), sm, vm); err != nil {
			t.Fatalf("restorePhase: %v", err)
		}
		smtest.Check(t, sm).In(phasePending).NoDeadline()
	})

	t.Run("a persisted deadline is restored as it stands", func(t *testing.T) {
		vm := validatingVM("")
		deadline := metav1.NewTime(time.Now().Add(90 * time.Second))
		vm.Status.PhaseDeadline = &deadline
		p := &migrationPass{vm: vm}
		sm := r.machineFor(t.Context(), p)
		defer sm.Close()
		if err := r.restorePhase(t.Context(), sm, vm); err != nil {
			t.Fatalf("restorePhase: %v", err)
		}
		smtest.Check(t, sm).In(phaseValidating).DeadlineIn(90*time.Second, 2*time.Second)
	})

	t.Run("a deadline in the past restores as a timeout", func(t *testing.T) {
		vm := validatingVM("")
		vm.Status.PhaseDeadline = expiredPhase()
		p := &migrationPass{vm: vm}
		sm := r.machineFor(t.Context(), p)
		defer sm.Close()
		if err := r.restorePhase(t.Context(), sm, vm); err != nil {
			t.Fatalf("restorePhase: %v", err)
		}
		smtest.Check(t, sm).In(phaseValidating).TimedOut()
	})

	t.Run("a bounded phase without a deadline is re-armed", func(t *testing.T) {
		vm := validatingVM("")
		vm.Status.PhaseDeadline = nil
		p := &migrationPass{vm: vm}
		sm := r.machineFor(t.Context(), p)
		defer sm.Close()
		if err := r.restorePhase(t.Context(), sm, vm); err != nil {
			t.Fatalf("restorePhase: %v", err)
		}
		smtest.Check(t, sm).In(phaseValidating).
			DeadlineIn(phaseBound(phaseValidating), 2*time.Second).NotTimedOut()
	})

	t.Run("an unbounded phase without a deadline stays unbounded", func(t *testing.T) {
		vm := runningVM(nil)
		p := &migrationPass{vm: vm}
		sm := r.machineFor(t.Context(), p)
		defer sm.Close()
		if err := r.restorePhase(t.Context(), sm, vm); err != nil {
			t.Fatalf("restorePhase: %v", err)
		}
		smtest.Check(t, sm).In(phaseRunning).NoDeadline()
	})

	t.Run("an unrecognised phase is an error, not a fresh start", func(t *testing.T) {
		vm := baseVM()
		vm.Status.Phase = simplyblockv1alpha1.VolumeMigrationPhase("Teleporting")
		p := &migrationPass{vm: vm}
		sm := r.machineFor(t.Context(), p)
		defer sm.Close()
		if err := r.restorePhase(t.Context(), sm, vm); err == nil {
			t.Fatalf("restorePhase accepted an undeclared phase; the migration would restart")
		}
	})
}

// A phase and its deadline are written together. A phase persisted without its
// deadline is a phase that can never time out, and one that kept a stale deadline
// after moving on would time out the wrong phase.
func TestPersistWritesPhaseAndDeadlineTogether(t *testing.T) {
	t.Run("a bounded phase keeps its deadline", func(t *testing.T) {
		vm := validatingVM("")
		vm.Status.PhaseDeadline = nil
		r, cl := newVMReconciler(t, ctrltest.UnreachableAPI, vm)

		// A step that does nothing: restorePhase re-arms the Validating bound, and
		// persisting has to carry it back out.
		if err := runStep(t, r, vm, noStep); err != nil {
			t.Fatalf("runStep: %v", err)
		}

		got := getVM(t, cl)
		if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseValidating {
			t.Fatalf("phase = %q, want Validating", got.Status.Phase)
		}
		if got.Status.PhaseDeadline == nil {
			t.Fatalf("PhaseDeadline is unset; the phase could never time out")
		}
		if remaining := time.Until(got.Status.PhaseDeadline.Time); remaining <= 0 {
			t.Errorf("PhaseDeadline is %v in the past, want the Validating bound ahead", -remaining)
		}
	})

	t.Run("an unbounded phase drops the previous deadline", func(t *testing.T) {
		vm := validatingVM("")
		deadline := metav1.NewTime(time.Now().Add(time.Minute))
		vm.Status.PhaseDeadline = &deadline
		r, cl := newVMReconciler(t, ctrltest.UnreachableAPI, vm)

		// Failed has no bound, so the deadline that bounded Validating must not follow
		// the migration into it.
		err := runStep(t, r, vm, func(ctx context.Context, p *migrationPass, sm *statemachine.Machine[phase]) (ctrl.Result, error) {
			return r.fail(ctx, p, sm, "validation gave up")
		})
		if err != nil {
			t.Fatalf("fail: %v", err)
		}

		got := getVM(t, cl)
		if got.Status.Phase != simplyblockv1alpha1.VolumeMigrationPhaseFailed {
			t.Fatalf("phase = %q, want Failed", got.Status.Phase)
		}
		if got.Status.PhaseDeadline != nil {
			t.Errorf("PhaseDeadline = %v, want it cleared for a terminal phase", got.Status.PhaseDeadline)
		}
		if got.Status.ErrorMessage != "validation gave up" {
			t.Errorf("ErrorMessage = %q, want the reason the step gave", got.Status.ErrorMessage)
		}
	})
}

// noStep is a step that does nothing, for asserting on what Reconcile does around one.
func noStep(_ context.Context, _ *migrationPass, _ *statemachine.Machine[phase]) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// requeueWithin never introduces a requeue and never extends one: a step waiting on a
// watch must keep waiting on it, and a step's own delay is a ceiling, not a target.
func TestRequeueWithin(t *testing.T) {
	sm := graphMachine(t)
	if err := sm.TransitionTo(t.Context(), phasePending); err != nil {
		t.Fatalf("arm the Pending bound: %v", err)
	}
	smtest.Check(t, sm).DeadlineIn(maxMigrationDeferral, 2*time.Second)

	t.Run("no requeue stays no requeue", func(t *testing.T) {
		if got := requeueWithin(ctrl.Result{}, sm); (got != ctrl.Result{}) {
			t.Errorf("requeueWithin = %+v, want the watch-driven empty result", got)
		}
	})

	t.Run("a delay past the bound is shortened to it", func(t *testing.T) {
		got := requeueWithin(ctrl.Result{RequeueAfter: 2 * maxMigrationDeferral}, sm)
		if got.RequeueAfter <= 0 || got.RequeueAfter > maxMigrationDeferral {
			t.Errorf("RequeueAfter = %v, want it clamped to at most %v", got.RequeueAfter, maxMigrationDeferral)
		}
	})

	t.Run("a delay inside the bound is left alone", func(t *testing.T) {
		got := requeueWithin(ctrl.Result{RequeueAfter: 5 * time.Second}, sm)
		if got.RequeueAfter != 5*time.Second {
			t.Errorf("RequeueAfter = %v, want the step's own 5s", got.RequeueAfter)
		}
	})

	t.Run("an unbounded phase keeps the step's delay", func(t *testing.T) {
		sm := graphMachine(t)
		if err := sm.Restore(statemachine.Snapshot[phase]{State: phaseRunning}); err != nil {
			t.Fatalf("restore Running: %v", err)
		}
		got := requeueWithin(ctrl.Result{RequeueAfter: time.Hour}, sm)
		if got.RequeueAfter != time.Hour {
			t.Errorf("RequeueAfter = %v, want the step's own hour", got.RequeueAfter)
		}
	})
}
