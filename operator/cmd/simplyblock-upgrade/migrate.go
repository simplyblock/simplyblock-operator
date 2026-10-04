// Phase two. §15 states what it is for: perform the actual application-level
// migration from the old resource model to the new one. Unlike API conversion,
// these operations change the semantic resource model, and §26 is explicit that
// this is the point after which rollback of the application data model is no
// longer guaranteed.
//
// So the command confirms before it starts, and everything after that is the
// walk of §23 driven by the framework.

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// newMigrateCommand builds `simplyblock-upgrade migrate`.
func newMigrateCommand(global *globalOptions) *cobra.Command {
	var (
		dryRun bool
		yes    bool
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Perform the application-level resource migration",
		Long: "migrate transforms the resource model: it copies the renamed " +
			"and absorbed kinds, reparents what a retired owner holds, " +
			"normalizes the volume handles whose pool segment carries a name, " +
			"deletes what its replacements have made obsolete, and rewrites " +
			"every object of the converting kinds into the new storage " +
			"version.\n\n" +
			"It is idempotent and resumes a run that was interrupted. It is " +
			"also the point after which rollback of the application data model " +
			"is no longer guaranteed, so it asks before it starts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			if !dryRun && !yes {
				if err := confirm(); err != nil {
					return err
				}
			}

			session, err := newSession(global, upgrade.StageMigrate, dryRun)
			if err != nil {
				return err
			}
			defer session.Close() //nolint:errcheck // giving the terminal back cannot fail the run

			if err := session.Runner.Discover(ctx); err != nil {
				return err
			}

			store := upgrade.NewRecordStore(session.Client, global.Namespace)
			return upgrade.NewMigration(session.Runner, store).Run(ctx)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be done and write nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

// confirm is the point of no return §26 requires be stated before the phase
// runs. It reads from the terminal rather than trusting a flag, and a run with
// no terminal has to pass --yes, which is what a Job does.
func confirm() error {
	fmt.Println()
	fmt.Println("migrate changes the resource model, and it is the point after which")
	fmt.Println("rollback of the application data model is no longer guaranteed.")
	fmt.Println()
	fmt.Println("Before it runs, the cluster should have been verified against the new")
	fmt.Println("operator: the control plane healthy, the storage cluster serving, and")
	fmt.Println("workloads behaving as they did.")
	fmt.Println()
	fmt.Print("Type 'migrate' to continue: ")

	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("nothing to read a confirmation from: pass --yes to run unattended")
	}
	if strings.TrimSpace(answer) != "migrate" {
		return fmt.Errorf("not confirmed, and nothing was changed")
	}
	return nil
}
