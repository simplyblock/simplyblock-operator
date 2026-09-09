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
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/spine"
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

			// The spine is printed before the findings, so the objects a
			// finding names have already been shown in the structure it is
			// about (§17).
			session.Reporter.Block("The ownership spine, and what the migration does to it",
				spine.Render(spine.Build(session.Runner.Scope)))

			// The plan is for the stage the cluster is positioned for, which is
			// read from the cluster rather than passed by the user (§27).
			position, err := upgrade.Positioned(ctx, session.Runner.Scope)
			if err != nil {
				return err
			}
			session.Runner.Scope.Stage = position.Stage
			session.Reporter.Block(
				fmt.Sprintf("This cluster is positioned for %s", position.Stage),
				[]string{position.Because})

			plan, err := session.Runner.Plan(ctx, position.Stage)
			if err != nil {
				return err
			}
			session.Reporter.Plan(plan)

			// A stage whose steps this build does not carry produces an empty
			// plan, and an empty plan and a cluster with nothing to do look
			// identical. Saying which is the difference between a report and a
			// silence.
			steps, err := session.Runner.Catalog.StepsFor(position.Stage, session.Runner.Scope.Options)
			if err == nil && len(steps) == 0 {
				session.Reporter.Progress(
					"no %s steps are registered in this build, so the plan above is empty "+
						"because nothing implements that stage yet, not because there is nothing to do",
					position.Stage)
			}

			if plan.Blocked() {
				return fmt.Errorf("preflight failed: %d violations, and no changes were made",
					len(plan.Findings.Errors()))
			}
			return nil
		},
	}
}
