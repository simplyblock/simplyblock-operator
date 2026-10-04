// simplyblock-upgrade migrates a simplyblock installation across a breaking
// change to the storage.simplyblock.io API. It has three commands, and the
// separation between them is the design's:
//
//	preflight  Read-only. Reports the checks and the plan, and changes nothing.
//	upgrade    Makes the cluster capable of running the new operator, leaving
//	           the existing resource model intact.
//	migrate    Performs the application-level migration, after the user has had
//	           an opportunity to verify the new operator.
//
// operator/docs/designs/crd-redesign/design-api-upgrade.md is the design, and
// internal/upgrade is the framework the three commands are assembled from.
//
// It is built on its own (`make -C operator build-upgrade`) and is not in the
// operator image: it is a prerequisite for running the new operator, so
// shipping it inside that operator's image would put it behind the pull it
// precedes. It reaches a cluster through a kubeconfig, or through the
// in-cluster configuration when it runs as a Job.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		// Cobra has already printed the error. Exiting non-zero is what a Job
		// and a shell read, and printing it twice is what makes a failed
		// migration harder to read than it has to be.
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
}
