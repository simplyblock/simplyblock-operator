package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/simplyblock/simplyblock-operator/internal/volumemigration"
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

	// Connect the new paths. If already connected, this is a no-op.
	if err := volumemigration.EnsureMigrationPaths(conns); err != nil {
		log.Fatalf("ensure migration paths: %v", err)
	}

	// Run a validation on the new paths. Failing to find all required inaccessible paths is a failure.
	if err := volumemigration.ValidateMigrationPaths(conns); err != nil {
		log.Fatalf("validation failed: %v", err)
	}
	log.Println("All paths validated: inaccessible paths present")
}