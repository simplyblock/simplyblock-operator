package main

import (
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

func validateMigration() {
	data := os.Getenv("VMIG_CONNECTIONS")
	if data == "" {
		fmt.Fprintln(os.Stderr, "VMIG_CONNECTIONS env var not set")
		os.Exit(1)
	}
	var conns []volumemigration.Connection
	if err := json.Unmarshal([]byte(data), &conns); err != nil {
		fmt.Fprintf(os.Stderr, "parse VMIG_CONNECTIONS: %v\n", err)
		os.Exit(1)
	}

	attempts := validateAttempts()
	delay := validateRetryDelay()

	// The freshly-connected target path can lag behind: nvme connect may return
	// before its controller is enumerated in `nvme list` and the ANA log page
	// settles. Retry the connect+validate cycle a few times before giving up so
	// a transient enumeration lag is not mistaken for a missing path. Already
	// connected paths are a no-op in EnsureMigrationPaths, so re-running it only
	// re-attempts paths that are genuinely missing.
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := volumemigration.EnsureMigrationPaths(conns); err != nil {
			lastErr = fmt.Errorf("ensure migration paths: %w", err)
		} else if err := volumemigration.ValidateMigrationPaths(conns); err != nil {
			lastErr = fmt.Errorf("validation: %w", err)
		} else {
			log.Printf("All paths validated: inaccessible paths present (attempt %d/%d)", attempt, attempts)
			return
		}

		log.Printf("path validation attempt %d/%d failed: %v", attempt, attempts, lastErr)
		if attempt < attempts {
			time.Sleep(delay)
		}
	}

	log.Fatalf("validation failed after %d attempt(s): %v", attempts, lastErr)
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