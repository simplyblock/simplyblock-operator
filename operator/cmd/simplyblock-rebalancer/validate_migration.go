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
	ensurePaths      func(conns []volumemigration.Connection) error
	verifyPaths      func(ctx context.Context, sysRoot, nqn string, conns []volumemigration.Connection,
		preExisting map[string]bool) ([]volumemigration.PathState, error)
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
		if err := v.ensurePaths(conns); err != nil {
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
	return outcomeValidated, fmt.Errorf("validation failed after %d attempt(s): %w", v.attempts, lastErr)
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
