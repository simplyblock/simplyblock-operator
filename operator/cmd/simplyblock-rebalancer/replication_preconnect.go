package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
)

// runReplicationPreconnect connects the target NVMe paths for a cross-cluster
// replication cutover during the cutover_pending window.
//
// The operator spawns this Job on every node consuming the replicating volume when
// the backend task enters the preconnect hold. By the time the backend's
// REPL_CUTOVER_PRECONNECT_WAIT_SEC deadline expires and run_cutover flips the ANA
// state (source → INACCESSIBLE, target → OPTIMIZED), the kernel already has the
// target controllers established and can route I/O there without any pod restart.
//
// Unlike validate-migration, this mode does NOT verify ANA state: the target clone
// is INACCESSIBLE before the ANA flip, which is expected and correct. All we need
// is the controller to exist so the kernel can switch to it immediately.
//
// Source paths are included in REPL_CONNECTIONS to be safe; EnsureMigrationPaths
// is idempotent for already-connected controllers, so existing source paths are
// left untouched.
//
// Environment variables:
//   - REPL_CONNECTIONS  — JSON array of volumemigration.Connection (required)
//   - VMIG_SYS_ROOT     — path to the host's /sys (empty = /sys)
func runReplicationPreconnect() {
	connsJSON := os.Getenv("REPL_CONNECTIONS")
	if connsJSON == "" {
		log.Fatal("replication-preconnect: REPL_CONNECTIONS env var not set")
	}

	var conns []volumemigration.Connection
	if err := json.Unmarshal([]byte(connsJSON), &conns); err != nil {
		log.Fatalf("replication-preconnect: parse REPL_CONNECTIONS: %v", err)
	}
	if len(conns) == 0 {
		log.Fatal("replication-preconnect: REPL_CONNECTIONS is empty")
	}

	sysRoot := os.Getenv("VMIG_SYS_ROOT")
	// All connections in one preconnect job share the same subsystem NQN.
	nqn := conns[0].NQN

	ctx := context.Background()
	log.Printf("replication-preconnect: connecting %d paths for %s (sysRoot=%q)", len(conns), nqn, sysRoot)

	// Clear any dead controllers left by a previous failed attempt. A half-connected
	// controller with no live namespace blocks a fresh nvme-connect for the same
	// address; clearing it here prevents one bad Job from poisoning subsequent ones.
	if reaped, err := volumemigration.ReapDeadControllers(ctx, sysRoot, nqn); err != nil {
		log.Printf("replication-preconnect: reap dead controllers: %v (continuing)", err)
	} else if len(reaped) > 0 {
		log.Printf("replication-preconnect: reaped %d dead controller(s): %s",
			len(reaped), volumemigration.FormatReleased(reaped))
	}

	const (
		attempts   = 3
		retryDelay = 5 * time.Second
	)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := volumemigration.EnsureMigrationPaths(ctx, sysRoot, conns); err != nil {
			lastErr = err
			log.Printf("replication-preconnect: attempt %d/%d failed: %v", attempt, attempts, err)
			if attempt < attempts {
				time.Sleep(retryDelay)
			}
			continue
		}
		log.Printf("replication-preconnect: %d paths connected (attempt %d/%d)", len(conns), attempt, attempts)
		return
	}
	log.Fatalf("replication-preconnect: failed after %d attempt(s): %v", attempts, lastErr)
}
