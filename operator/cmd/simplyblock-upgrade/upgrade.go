// Phase one. §9 states what it is for: make the cluster capable of running the
// new operator without performing the application-level resource migration, and
// perform no destructive change to the existing resource topology.
//
// The command owns none of that. It runs the steps registered for the stage,
// and the guarantee is the framework's: a step is planned, skipped when its
// effect is already present, applied, and then verified.

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// newUpgradeCommand builds `simplyblock-upgrade upgrade`.
func newUpgradeCommand(global *globalOptions) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Make the cluster capable of running the new operator",
		Long: "upgrade deploys the conversion webhook, installs the new CRDs, " +
			"hands the Helm release over so the upgrade prunes nothing, and " +
			"upgrades the operator.\n\n" +
			"It leaves the existing resource model intact, so the cluster can " +
			"be verified before migrate is run. It refuses to start if the " +
			"preflight checks do not pass.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			session, err := newSession(global, upgrade.StageUpgrade, dryRun)
			if err != nil {
				return err
			}
			defer session.Close() //nolint:errcheck // giving the terminal back cannot fail the run

			if err := session.Runner.Discover(ctx); err != nil {
				return err
			}

			// The checks run before anything is deployed or changed, which is
			// what §9.2 requires: a violation found here leaves the cluster
			// untouched.
			findings, err := session.Runner.Check(ctx, upgrade.StageUpgrade)
			if err != nil {
				return err
			}
			session.Reporter.Findings(findings)
			if findings.Blocked() {
				return fmt.Errorf("upgrade refused: %d violations, and no changes were made",
					len(findings.Errors()))
			}

			return session.Runner.ApplyAll(ctx, upgrade.StageUpgrade)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be done and write nothing")
	return cmd
}
