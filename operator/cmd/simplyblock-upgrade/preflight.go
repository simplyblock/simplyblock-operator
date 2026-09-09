// The read-only command. §27 gives it both halves of the answer, the checks and
// the plan, because it already discovers the graph and validates it, and
// calculating the transformations is that same walk with the writes left out.
// One command that reads therefore means one implementation of the walk that
// reads.

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// newPreflightCommand builds `simplyblock-upgrade preflight`.
func newPreflightCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "Report the checks and the plan, changing nothing",
		Long: "preflight reads the cluster and reports two things: every check " +
			"the upgrade would make, and the plan for whichever phase the " +
			"cluster is positioned for.\n\n" +
			"It writes nothing, not even a server-side dry run, so it is safe " +
			"to run at any time. The name and identity checks are the ones " +
			"worth running early: a violation there is sometimes resolved by a " +
			"data migration rather than by an edit, and found here the cluster " +
			"is still untouched.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			// The preflight runs against a client that refuses writes, so a
			// check that writes fails the run rather than changing a cluster
			// the user was told nothing would change on.
			session, err := newSession(global, upgrade.StagePreflight, true)
			if err != nil {
				return err
			}
			defer session.Close() //nolint:errcheck // giving the terminal back cannot fail the run

			if err := session.Runner.Discover(ctx); err != nil {
				return err
			}

			// The plan is reported for the stage the cluster is positioned
			// for, which is derived from the cluster rather than passed by the
			// user (§22.1, §27).
			stage := upgrade.StageUpgrade
			session.Runner.Scope.Stage = stage

			plan, err := session.Runner.Plan(ctx, stage)
			if err != nil {
				return err
			}
			session.Reporter.Plan(plan)

			if plan.Blocked() {
				return fmt.Errorf("preflight failed: %d violations, and no changes were made",
					len(plan.Findings.Errors()))
			}
			return nil
		},
	}
}
