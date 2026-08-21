package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// The retry budget is what keeps a transient enumeration lag from failing a healthy
// migration, and it must stay inside the Job's deadline — so a bad override has to
// fall back to the default rather than becoming, say, an hour.
func TestValidateAttempts(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", defaultValidateAttempts},
		{"valid override", "5", 5},
		{"one attempt", "1", 1},
		{"zero falls back", "0", defaultValidateAttempts},
		{"negative falls back", "-3", defaultValidateAttempts},
		{"garbage falls back", "many", defaultValidateAttempts},
		{"float falls back", "2.5", defaultValidateAttempts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VMIG_VALIDATE_ATTEMPTS", tc.env)
			if got := validateAttempts(); got != tc.want {
				t.Errorf("validateAttempts with %q = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

func TestValidateRetryDelay(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset", "", defaultValidateRetryDelay},
		{"valid duration", "5s", 5 * time.Second},
		{"sub-second", "500ms", 500 * time.Millisecond},
		// Zero is accepted here, unlike zero attempts: "retry immediately" is a
		// meaningful setting, whereas zero attempts would skip validation entirely.
		{"zero means no delay", "0s", 0},
		{"negative falls back", "-2s", defaultValidateRetryDelay},
		{"unitless falls back", "2", defaultValidateRetryDelay},
		{"garbage falls back", "soon", defaultValidateRetryDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VMIG_VALIDATE_RETRY_DELAY", tc.env)
			if got := validateRetryDelay(); got != tc.want {
				t.Errorf("validateRetryDelay with %q = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// ---- connection parsing ----

func TestParseConnections(t *testing.T) {
	t.Run("valid list", func(t *testing.T) {
		conns, err := parseConnections(
			`[{"nqn":"nqn.x","ip":"10.0.0.1","port":4420,"transport":"tcp","ctrlLossTmo":3600}]`)
		if err != nil {
			t.Fatalf("parseConnections: %v", err)
		}
		if len(conns) != 1 || conns[0].NQN != "nqn.x" || conns[0].Port != 4420 || conns[0].CtrlLossTmo != 3600 {
			t.Errorf("connections = %+v, want the decoded path", conns)
		}
	})

	// The job is only ever started with paths to establish, so an empty or broken
	// variable means the environment is wrong — better to fail than to "validate"
	// nothing and report success.
	t.Run("unset", func(t *testing.T) {
		if _, err := parseConnections(""); err == nil {
			t.Errorf("expected an error for an unset VMIG_CONNECTIONS")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		if _, err := parseConnections(`[{"nqn":`); err == nil {
			t.Errorf("expected a parse error")
		}
	})

	t.Run("wrong shape", func(t *testing.T) {
		if _, err := parseConnections(`{"nqn":"nqn.x"}`); err == nil {
			t.Errorf("expected an error for an object where a list is required")
		}
	})
}

// ---- the run flow ----

// recorder counts the host operations a run performs.
type recorder struct {
	lookups        int
	ensures        int
	validates      int
	sleeps         []time.Duration
	present        bool
	lookupErr      error
	ensureErrs     []error
	validateErrs   []error
	preExisting    map[string]bool
	presentErr     error
	ensureCallSeq  int
	validateCallSq int

	reaps       int
	releases    int
	releaseConn []volumemigration.Connection
	reapErr     error
	releaseErr  error
	// order records the host-touching steps in the order they happened, so the two
	// cleanups can be pinned to the right side of the connect.
	order []string
}

func (rec *recorder) newRun(attempts int) validationRun {
	return validationRun{
		hostHasSubsystem: func(_ context.Context, _, _ string) (bool, error) {
			rec.lookups++
			return rec.present, rec.lookupErr
		},
		ensurePaths: func(context.Context, string, []volumemigration.Connection) error {
			rec.ensures++
			rec.order = append(rec.order, "ensure")
			err := errAt(rec.ensureErrs, rec.ensureCallSeq)
			rec.ensureCallSeq++
			return err
		},
		reapDead: func(_ context.Context, _, _ string) ([]volumemigration.Released, error) {
			rec.reaps++
			rec.order = append(rec.order, "reap")
			return nil, rec.reapErr
		},
		releasePaths: func(_ context.Context, _, _ string,
			c []volumemigration.Connection) ([]volumemigration.Released, error) {
			rec.releases++
			rec.releaseConn = c
			rec.order = append(rec.order, "release")
			return nil, rec.releaseErr
		},
		presentAddresses: func(_ context.Context, _, _ string) (map[string]bool, error) {
			return rec.preExisting, rec.presentErr
		},
		verifyPaths: func(_ context.Context, _, _ string, _ []volumemigration.Connection,
			_ map[string]bool) ([]volumemigration.PathState, error) {
			rec.validates++
			err := errAt(rec.validateErrs, rec.validateCallSq)
			rec.validateCallSq++
			return nil, err
		},
		sleep:    func(d time.Duration) { rec.sleeps = append(rec.sleeps, d) },
		attempts: attempts,
		delay:    time.Millisecond,
	}
}

// errAt returns the error scripted for call i, or nil once the script runs out.
func errAt(errs []error, i int) error {
	if i < len(errs) {
		return errs[i]
	}
	return nil
}

var conns = []volumemigration.Connection{{NQN: "nqn.x", IP: "10.0.0.1", Port: 4420, Transport: "tcp"}}

func TestValidationRun_SubsystemGate(t *testing.T) {
	const nqn = "nqn.2023-02.io.simplyblock:cluster:lvol:vol-1"

	// The gate is skipped entirely when the operator passes no NQN — an older operator,
	// or a deliberate opt-out — and validation runs as before.
	t.Run("no NQN means no gate", func(t *testing.T) {
		rec := &recorder{}
		outcome, err := rec.newRun(3).run(context.Background(), "", "", conns)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if outcome != outcomeValidated {
			t.Errorf("outcome = %v, want validated", outcome)
		}
		if rec.lookups != 0 {
			t.Errorf("performed %d subsystem lookups, want none without an NQN", rec.lookups)
		}
		if rec.ensures != 1 || rec.validates != 1 {
			t.Errorf("ensure/validate = %d/%d, want 1/1", rec.ensures, rec.validates)
		}
	})

	t.Run("connected host validates", func(t *testing.T) {
		rec := &recorder{present: true}
		outcome, err := rec.newRun(3).run(context.Background(), "/host/sys", nqn, conns)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if outcome != outcomeValidated {
			t.Errorf("outcome = %v, want validated", outcome)
		}
		if rec.ensures != 1 || rec.validates != 1 {
			t.Errorf("ensure/validate = %d/%d, want 1/1", rec.ensures, rec.validates)
		}
	})

	// The important negative: a node with no connection must be left completely alone.
	// Connecting there would strand a controller for a subsystem it does not use.
	t.Run("unconnected host is skipped without touching it", func(t *testing.T) {
		rec := &recorder{present: false}
		outcome, err := rec.newRun(3).run(context.Background(), "/host/sys", nqn, conns)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if outcome != outcomeSkipped {
			t.Errorf("outcome = %v, want skipped", outcome)
		}
		if rec.ensures != 0 || rec.validates != 0 {
			t.Errorf("ensure/validate = %d/%d, want 0/0 — an unconnected node must not be touched",
				rec.ensures, rec.validates)
		}
	})

	// An untrustworthy lookup must fail the job. Treating it as "absent" would let the
	// operator cut over believing this node had been handled.
	t.Run("lookup error fails the run", func(t *testing.T) {
		rec := &recorder{lookupErr: errors.New("sysfs not visible")}
		_, err := rec.newRun(3).run(context.Background(), "/host/sys", nqn, conns)
		if err == nil {
			t.Fatalf("expected an error when the lookup cannot be trusted")
		}
		if !strings.Contains(err.Error(), nqn) || !strings.Contains(err.Error(), "sysfs not visible") {
			t.Errorf("error = %q, want the NQN and the cause", err)
		}
		if rec.ensures != 0 {
			t.Errorf("connected %d path(s) despite an unusable lookup", rec.ensures)
		}
	})
}

func TestValidationRun_RetryLoop(t *testing.T) {
	// A connect that lands but is not yet enumerated is the normal case the retry
	// exists for: a later attempt must be allowed to succeed.
	t.Run("succeeds on a later attempt", func(t *testing.T) {
		rec := &recorder{present: true, validateErrs: []error{errors.New("no inaccessible path")}}
		outcome, err := rec.newRun(3).run(context.Background(), "", "", conns)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if outcome != outcomeValidated {
			t.Errorf("outcome = %v, want validated", outcome)
		}
		if rec.validates != 2 {
			t.Errorf("validated %d times, want 2 (failed once, then succeeded)", rec.validates)
		}
		if len(rec.sleeps) != 1 {
			t.Errorf("slept %d times, want once between the two attempts", len(rec.sleeps))
		}
	})

	t.Run("exhausts the budget and reports the last error", func(t *testing.T) {
		boom := errors.New("no inaccessible path found for: nqn.x@10.0.0.1:4420")
		rec := &recorder{present: true, validateErrs: []error{boom, boom, boom}}
		_, err := rec.newRun(3).run(context.Background(), "", "", conns)
		if err == nil {
			t.Fatalf("expected an error after the attempts were exhausted")
		}
		if !strings.Contains(err.Error(), "3 attempt(s)") || !strings.Contains(err.Error(), boom.Error()) {
			t.Errorf("error = %q, want the attempt count and the last cause", err)
		}
		if rec.validates != 3 {
			t.Errorf("validated %d times, want 3", rec.validates)
		}
		// No sleep after the final attempt — that would just delay the failure.
		if len(rec.sleeps) != 2 {
			t.Errorf("slept %d times, want 2 for 3 attempts", len(rec.sleeps))
		}
	})

	t.Run("a single attempt never sleeps", func(t *testing.T) {
		rec := &recorder{present: true, validateErrs: []error{errors.New("nope")}}
		if _, err := rec.newRun(1).run(context.Background(), "", "", conns); err == nil {
			t.Fatalf("expected an error")
		}
		if len(rec.sleeps) != 0 {
			t.Errorf("slept %d times with a single attempt, want none", len(rec.sleeps))
		}
	})

	// The two failure sources are reported distinctly: a connect that never happened
	// is a different problem from a path that came up in the wrong state.
	t.Run("connect failure is distinguishable from validation failure", func(t *testing.T) {
		rec := &recorder{present: true, ensureErrs: []error{
			errors.New("nvme connect: no route to host"),
			errors.New("nvme connect: no route to host"),
		}}
		_, err := rec.newRun(2).run(context.Background(), "", "", conns)
		if err == nil {
			t.Fatalf("expected an error")
		}
		if !strings.Contains(err.Error(), "ensure migration paths") {
			t.Errorf("error = %q, want it attributed to the connect step", err)
		}
		if rec.validates != 0 {
			t.Errorf("validated %d times, want 0 — paths were never connected", rec.validates)
		}
	})

	// An empty connection list is not an error at this level: the paths are whatever
	// the control plane returned, and validation of nothing trivially holds.
	t.Run("no connections", func(t *testing.T) {
		rec := &recorder{present: true}
		if _, err := rec.newRun(3).run(context.Background(), "", "", nil); err != nil {
			t.Errorf("run with no connections: %v", err)
		}
	})
}

// The pre-existing snapshot is taken before connecting, so verification can tell a path
// we created from one that was already there. If that snapshot cannot be read, the run
// must fail rather than verify against an unknown baseline.
func TestValidationRun_PreExistingSnapshotFails(t *testing.T) {
	rec := &recorder{present: true, presentErr: errors.New("sysfs unreadable")}
	_, err := rec.newRun(3).run(context.Background(), "/host/sys", "nqn.x", conns)
	if err == nil {
		t.Fatalf("expected an error when the baseline cannot be read")
	}
	if !strings.Contains(err.Error(), "existing paths") {
		t.Errorf("error = %q, want it to name the baseline read", err)
	}
	if rec.ensures != 0 {
		t.Errorf("connected %d path(s) without a baseline", rec.ensures)
	}
}

// ---- cleanup around the run ----

// The leak that poisoned the test cluster: a validation that fails must give its target
// paths back, on the node that has them, before the exit code cancels the migration.
func TestValidationRun_ReleasesPathsWhenValidationFails(t *testing.T) {
	boom := errors.New("controller-not-contributing")
	rec := &recorder{present: true, validateErrs: []error{boom, boom}}
	if _, err := rec.newRun(2).run(context.Background(), "/host/sys", "nqn.x", conns); err == nil {
		t.Fatal("expected the validation to fail")
	}
	if rec.releases != 1 {
		t.Errorf("released %d times, want exactly once after the attempts were exhausted", rec.releases)
	}
	if len(rec.releaseConn) != len(conns) || rec.releaseConn[0].IP != conns[0].IP {
		t.Errorf("released %v, want the migration's own target connections", rec.releaseConn)
	}
}

// A validation that passes must not release: those paths are about to become the
// volume's data path at cutover, and releasing them is the outage being avoided.
func TestValidationRun_KeepsPathsWhenValidationPasses(t *testing.T) {
	rec := &recorder{present: true}
	if _, err := rec.newRun(3).run(context.Background(), "/host/sys", "nqn.x", conns); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.releases != 0 {
		t.Errorf("released %d time(s) after a successful validation, want none", rec.releases)
	}
}

// The reap runs once, before anything is connected: it clears an earlier migration's
// husks so this one's verification is not rejected over them. Doing it after the connect
// would diagnose the paths this run just established.
func TestValidationRun_ReapsBeforeConnecting(t *testing.T) {
	rec := &recorder{present: true}
	if _, err := rec.newRun(3).run(context.Background(), "/host/sys", "nqn.x", conns); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.reaps != 1 {
		t.Errorf("reaped %d times, want exactly once", rec.reaps)
	}
	if len(rec.order) == 0 || rec.order[0] != "reap" {
		t.Errorf("step order = %v, want the reap first", rec.order)
	}
}

// A node with no connection to the subsystem is skipped before either cleanup: it has no
// paths of ours, and connecting or reaping there would touch a subsystem it does not use.
func TestValidationRun_SkippedNodeIsNotTouched(t *testing.T) {
	rec := &recorder{present: false}
	outcome, err := rec.newRun(3).run(context.Background(), "/host/sys", "nqn.x", conns)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if outcome != outcomeSkipped {
		t.Fatalf("outcome = %v, want skipped", outcome)
	}
	if rec.reaps != 0 || rec.releases != 0 {
		t.Errorf("reaped %d and released %d on a node with no connection, want neither",
			rec.reaps, rec.releases)
	}
}

// Neither cleanup may change the outcome. A reap that fails still lets the validation
// decide, and a release that fails must not mask why the migration was cancelled.
func TestValidationRun_CleanupFailuresDoNotChangeTheOutcome(t *testing.T) {
	t.Run("a failed reap still validates", func(t *testing.T) {
		rec := &recorder{present: true, reapErr: errors.New("delete_controller: device busy")}
		if _, err := rec.newRun(3).run(context.Background(), "/host/sys", "nqn.x", conns); err != nil {
			t.Errorf("run: %v, want the failed reap to be swallowed", err)
		}
	})

	t.Run("a failed release still reports the validation error", func(t *testing.T) {
		boom := errors.New("no inaccessible path")
		rec := &recorder{
			present:      true,
			validateErrs: []error{boom},
			releaseErr:   errors.New("delete_controller: device busy"),
		}
		_, err := rec.newRun(1).run(context.Background(), "/host/sys", "nqn.x", conns)
		if err == nil {
			t.Fatal("expected the validation error")
		}
		if !strings.Contains(err.Error(), boom.Error()) {
			t.Errorf("error = %q, want the validation cause, not the release failure", err)
		}
	})
}

// The baseline is passed through to verification, which is what lets it report whether
// our connect contributed anything on an HA cluster where the target already listens.
func TestValidationRun_PassesPreExistingToVerification(t *testing.T) {
	var got map[string]bool
	rec := &recorder{present: true, preExisting: map[string]bool{"10.0.0.112:4428": true}}
	run := rec.newRun(1)
	run.verifyPaths = func(_ context.Context, _, _ string, _ []volumemigration.Connection,
		pre map[string]bool) ([]volumemigration.PathState, error) {
		got = pre
		return nil, nil
	}
	if _, err := run.run(context.Background(), "/host/sys", "nqn.x", conns); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !got["10.0.0.112:4428"] {
		t.Errorf("verification received %v, want the pre-existing baseline", got)
	}
}
