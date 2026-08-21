package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

const (
	defaultValidateAttempts   = 3
	defaultValidateRetryDelay = 2 * time.Second
)

// validationOutcome is what a run of the validate-migration mode concluded.
type validationOutcome int

const (
	// outcomeValidated means the target paths are connected and in the expected
	// pre-cutover state on this host.
	outcomeValidated validationOutcome = iota
	// outcomeSkipped means this host holds no connection to the subsystem, so there
	// was nothing to switch over.
	outcomeSkipped
)

// validationRun is one run of the validate-migration mode with its host-touching
// operations injected, so the flow — the subsystem gate and the retry loop — can be
// exercised without a real NVMe host. main wires in the real implementations.
type validationRun struct {
	hostHasSubsystem func(ctx context.Context, sysRoot, nqn string) (bool, error)
	presentAddresses func(ctx context.Context, sysRoot, nqn string) (map[string]bool, error)
	ensurePaths      func(ctx context.Context, sysRoot string, conns []volumemigration.Connection) error
	verifyPaths      func(ctx context.Context, sysRoot, nqn string, conns []volumemigration.Connection,
		preExisting map[string]bool) ([]volumemigration.PathState, error)
	reapDead     func(ctx context.Context, sysRoot, nqn string) ([]volumemigration.Released, error)
	releasePaths func(ctx context.Context, sysRoot, nqn string,
		conns []volumemigration.Connection) ([]volumemigration.Released, error)
	sleep    func(time.Duration)
	attempts int
	delay    time.Duration
}

// newValidationRun returns a run wired to the real host operations, with the retry
// budget taken from the environment.
func newValidationRun() validationRun {
	return validationRun{
		hostHasSubsystem: volumemigration.HostHasSubsystem,
		presentAddresses: volumemigration.PresentAddresses,
		ensurePaths:      volumemigration.EnsureMigrationPaths,
		verifyPaths:      volumemigration.VerifyMigrationPaths,
		reapDead:         volumemigration.ReapDeadControllers,
		releasePaths:     volumemigration.ReleaseMigrationPaths,
		sleep:            time.Sleep,
		attempts:         validateAttempts(),
		delay:            validateRetryDelay(),
	}
}

// parseConnections decodes the connection list the operator passes in
// VMIG_CONNECTIONS. An empty value is an error: the job is only ever started with
// paths to establish, so an empty variable means the environment is wrong rather
// than that there is no work.
func parseConnections(data string) ([]volumemigration.Connection, error) {
	if data == "" {
		return nil, fmt.Errorf("VMIG_CONNECTIONS env var not set")
	}
	var conns []volumemigration.Connection
	if err := json.Unmarshal([]byte(data), &conns); err != nil {
		return nil, fmt.Errorf("parse VMIG_CONNECTIONS: %w", err)
	}
	return conns, nil
}

// run establishes and validates the migration's target paths on this host.
//
// When nqn is set it first asks whether this host is connected to that subsystem at
// all. A migration moves a whole NVMe subsystem, so this job runs on every node that
// consumes one of its volumes, and a node may no longer hold a connection by the time
// the job starts (its consumer pod went away). Connecting target paths there would
// leave a controller behind for a subsystem the node does not use, so an absent
// subsystem means skip. A lookup that cannot be trusted is an error, never "absent":
// the operator would otherwise cut over believing this node had been handled.
func (v validationRun) run(
	ctx context.Context,
	sysRoot, nqn string,
	conns []volumemigration.Connection,
) (validationOutcome, error) {
	if nqn != "" {
		present, err := v.hostHasSubsystem(ctx, sysRoot, nqn)
		if err != nil {
			return outcomeSkipped, fmt.Errorf(
				"cannot determine whether this host is connected to %s: %w", nqn, err)
		}
		if !present {
			return outcomeSkipped, nil
		}
		log.Printf("host is connected to %s; validating the migration paths", nqn)
	}

	// Clear the husks an earlier migration left behind before deciding anything about
	// this one. A validation run that never got to release its target paths leaves
	// controllers that are live and serve no namespace, and the verification below
	// rejects the migration over exactly those — so without this, one abandoned attempt
	// blocks every later migration of the subsystem for as long as the node is up. This
	// is the same defect the verification reports, cleared with the same diagnosis.
	//
	// Best effort: a reap that fails is logged and the run continues. The verification
	// is what decides whether the fabric is fit to cut over, and it is about to run.
	v.reap(ctx, sysRoot, nqn)

	// Which of the expected addresses the host already had a controller for. A path
	// that was there before proves nothing about our connect, and on an HA cluster the
	// migration target may already be a listener for this subsystem — so the check has
	// to know the difference rather than accept any matching path.
	preExisting, err := v.presentAddresses(ctx, sysRoot, nqn)
	if err != nil {
		return outcomeValidated, fmt.Errorf("read the host's existing paths to %s: %w", nqn, err)
	}
	for addr := range preExisting {
		log.Printf("path %s to %s already present before connecting", addr, nqn)
	}

	// The freshly-connected target path can lag behind: nvme connect may return before
	// its controller is live and the ANA log page settles. Retry the connect+verify
	// cycle a few times before giving up so a transient lag is not mistaken for a
	// missing path. Already connected paths are a no-op in ensurePaths, so re-running
	// it only re-attempts paths that are genuinely missing.
	var lastErr error
	for attempt := 1; attempt <= v.attempts; attempt++ {
		paths, verifyErr := []volumemigration.PathState(nil), error(nil)
		if err := v.ensurePaths(ctx, sysRoot, conns); err != nil {
			lastErr = fmt.Errorf("ensure migration paths: %w", err)
		} else if paths, verifyErr = v.verifyPaths(ctx, sysRoot, nqn, conns, preExisting); verifyErr != nil {
			lastErr = fmt.Errorf("verification: %w", verifyErr)
		} else {
			for _, p := range paths {
				log.Printf("path %s", p)
			}
			log.Printf("All paths verified: established, live and parked (attempt %d/%d)",
				attempt, v.attempts)
			return outcomeValidated, nil
		}

		for _, p := range paths {
			log.Printf("path %s", p)
		}
		log.Printf("path verification attempt %d/%d failed: %v", attempt, v.attempts, lastErr)
		if attempt < v.attempts {
			v.sleep(v.delay)
		}
	}

	// Give back the paths this run established. The migration is not going to cut over —
	// the operator cancels it on this Job's failure — so the target paths have no future
	// use, and leaving them is what made a single failed validation poison the host: they
	// stay connected, retry a target that has stopped answering for them, and settle into
	// the state the reap above now has to clear.
	//
	// Releasing here rather than leaving it to the operator is what makes it ordered: the
	// paths go before the exit code that cancels the migration, on the node that has them,
	// while this process still knows which addresses it was asked for. The operator's own
	// release covers the nodes that passed and are never told they failed.
	v.release(ctx, sysRoot, nqn, conns)

	return outcomeValidated, fmt.Errorf("validation failed after %d attempt(s): %w", v.attempts, lastErr)
}

// reap clears the dead controllers of nqn, logging what went. Failures are logged and
// swallowed: this runs to improve the odds of a verification that is about to happen
// anyway, and refusing to validate because a cleanup failed would turn a recoverable
// state into the outcome it was meant to prevent.
func (v validationRun) reap(ctx context.Context, sysRoot, nqn string) {
	if v.reapDead == nil || nqn == "" {
		return
	}
	reaped, err := v.reapDead(ctx, sysRoot, nqn)
	if len(reaped) > 0 {
		log.Printf("reaped %d dead controller(s) of %s before validating: %s",
			len(reaped), nqn, volumemigration.FormatReleased(reaped))
	}
	if err != nil {
		log.Printf("could not reap every dead controller of %s (continuing): %v", nqn, err)
	}
}

// release disconnects the migration's target paths, logging what went. Failures are
// logged and swallowed: the run has already failed and the exit code must report that
// failure rather than this one, which would only mask why the migration was cancelled.
func (v validationRun) release(
	ctx context.Context,
	sysRoot, nqn string,
	conns []volumemigration.Connection,
) {
	if v.releasePaths == nil || nqn == "" {
		return
	}
	released, err := v.releasePaths(ctx, sysRoot, nqn, conns)
	log.Printf("released %d migration target path(s) of %s: %s",
		len(released), nqn, volumemigration.FormatReleased(released))
	if err != nil {
		log.Printf("could not release every migration target path of %s: %v", nqn, err)
	}
}

func validateMigration() {
	conns, err := parseConnections(os.Getenv("VMIG_CONNECTIONS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	nqn := os.Getenv("VMIG_SUBSYSTEM_NQN")
	outcome, err := newValidationRun().run(context.Background(), os.Getenv("VMIG_SYS_ROOT"), nqn, conns)
	if err != nil {
		log.Fatal(err)
	}
	if outcome == outcomeSkipped {
		// The operator collects this log per node, so say plainly that this node
		// needed nothing rather than leaving an empty success.
		log.Printf("no host connection to %s on this node: nothing to validate", nqn)
	}
}

// releaseMigration gives back the migration target paths on this node without
// validating anything.
//
// It is the mode the operator runs on nodes whose own validation passed. Those never
// learn that the migration was cancelled — another node's Job failed, or the operator
// gave up waiting — so their Job exited successfully with the target paths connected and
// nothing on the node will ever release them. Every other failure path releases in the
// Job that failed; this one exists because a success cannot.
//
// Safe to run when there is nothing to do, which is the normal case: a subsystem that is
// not attached, or paths already gone, release nothing. It is also safe to run after a
// cutover the operator did not observe — the paths would be serving I/O by then, and
// ReleaseMigrationPaths will not touch a path that is serving.
func releaseMigration() {
	conns, err := parseConnections(os.Getenv("VMIG_CONNECTIONS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	nqn := os.Getenv("VMIG_SUBSYSTEM_NQN")
	if nqn == "" {
		log.Fatal("VMIG_SUBSYSTEM_NQN env var not set")
	}
	sysRoot := os.Getenv("VMIG_SYS_ROOT")

	released, err := volumemigration.ReleaseMigrationPaths(context.Background(), sysRoot, nqn, conns)
	log.Printf("released %d migration target path(s) of %s: %s",
		len(released), nqn, volumemigration.FormatReleased(released))
	if err != nil {
		log.Fatalf("could not release every migration target path of %s: %v", nqn, err)
	}

	// The husk a path lost before this ran leaves behind blocks the next migration of the
	// subsystem just as surely as the path would have, so clear it while we are here.
	reaped, rerr := volumemigration.ReapDeadControllers(context.Background(), sysRoot, nqn)
	if len(reaped) > 0 {
		log.Printf("reaped %d dead controller(s) of %s: %s",
			len(reaped), nqn, volumemigration.FormatReleased(reaped))
	}
	if rerr != nil {
		log.Printf("could not reap every dead controller of %s: %v", nqn, rerr)
	}
}

// validateAttempts returns the number of connect+validate attempts, overridable
// via VMIG_VALIDATE_ATTEMPTS. Invalid or non-positive values fall back to the default.
func validateAttempts() int {
	if v := os.Getenv("VMIG_VALIDATE_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("invalid VMIG_VALIDATE_ATTEMPTS=%q; using default %d", v, defaultValidateAttempts)
	}
	return defaultValidateAttempts
}

// validateRetryDelay returns the delay between attempts, overridable via
// VMIG_VALIDATE_RETRY_DELAY (a Go duration, e.g. "2s"). Invalid values fall back
// to the default.
func validateRetryDelay() time.Duration {
	if v := os.Getenv("VMIG_VALIDATE_RETRY_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
		log.Printf("invalid VMIG_VALIDATE_RETRY_DELAY=%q; using default %s", v, defaultValidateRetryDelay)
	}
	return defaultValidateRetryDelay
}
