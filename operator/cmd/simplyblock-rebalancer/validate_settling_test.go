// A path still being scanned for namespaces is waited for, not counted as a failed
// attempt: every batched migration used to log "attempt 1/3 failed" for it
// (2026-09-29, runs 13-17).

package main

import (
	"context"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

func settlingErr() error {
	return &volumemigration.NamespacesSettlingError{
		Paths:    []string{"10.50.0.236:4434 (1/5)"},
		Problems: []string{"controller-not-contributing: controller nvme12 serves no path to namespace 2"},
	}
}

func TestValidationRun_APartlyScannedPathIsWaitedForWithoutUsingAnAttempt(t *testing.T) {
	rec := &recorder{validateErrs: []error{settlingErr(), settlingErr()}}
	outcome, err := rec.newRun(1).run(context.Background(), "", "", conns)
	if err != nil {
		t.Fatalf("run: %v (a scan that finishes must not fail the only attempt)", err)
	}
	if outcome != outcomeValidated {
		t.Errorf("outcome = %v, want validated", outcome)
	}
	if rec.validates != 3 {
		t.Errorf("validates = %d, want 3 (two settling checks, then the pass)", rec.validates)
	}
	if rec.releases != 0 {
		t.Errorf("released paths %d time(s) on a run that passed", rec.releases)
	}
}

func TestValidationRun_AScanThatNeverFinishesStillFails(t *testing.T) {
	errs := make([]error, settleChecks+1)
	for i := range errs {
		errs[i] = settlingErr()
	}
	rec := &recorder{validateErrs: errs}
	if _, err := rec.newRun(1).run(context.Background(), "", "", conns); err == nil {
		t.Fatal("a path that never finishes its scan passed validation")
	}
	if rec.validates != settleChecks+1 {
		t.Errorf("validates = %d, want %d: the settle wait must be bounded", rec.validates, settleChecks+1)
	}
}
