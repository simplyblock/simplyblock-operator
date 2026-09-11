// The command tree and the flags every command shares. Cobra rather than the
// standard library's flag package, because the three commands take the same
// connection and selection flags and a flag set per command would let them
// drift.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// globalOptions are the flags every command takes.
type globalOptions struct {
	// Kubeconfig is the file to connect with. Empty means the in-cluster
	// configuration, then the default loading rules.
	Kubeconfig string

	// Context is the kubeconfig context to use, so a workstation holding
	// several clusters cannot upgrade the wrong one by default.
	Context string

	// Namespace is the installation being operated on.
	Namespace string

	// Skip names rules that are not to run. Every skipped rule is reported, so
	// the decision stays visible in the run's own output.
	Skip []string

	// AcknowledgeOffline admits a StorageNode that is not online. §18 refuses
	// on one by default, because a cluster that is already degraded should not
	// be asked to absorb an upgrade, and an operator who knows a node is down
	// and intends to proceed is making a call this flag records.
	AcknowledgeOffline bool

	// Plain forces the line-by-line reporter even at a terminal, which is what
	// a user redirecting output to a file wants.
	Plain bool

	// Verbose reports every rule as it runs rather than only the ones with
	// something to say.
	Verbose bool
}

// options returns the run's switches as the framework takes them.
func (g globalOptions) options(dryRun bool) upgrade.Options {
	skip := make([]upgrade.ID, 0, len(g.Skip))
	for _, id := range g.Skip {
		skip = append(skip, upgrade.ID(id))
	}
	return upgrade.Options{
		DryRun:             dryRun,
		Skip:               skip,
		AcknowledgeOffline: g.AcknowledgeOffline,
	}
}

// newRootCommand builds the command tree.
func newRootCommand() *cobra.Command {
	var global globalOptions

	root := &cobra.Command{
		Use:   "simplyblock-upgrade",
		Short: "Upgrade a simplyblock installation across a breaking API change",
		Long: "simplyblock-upgrade carries an installation from " +
			"storage.simplyblock.io/v1alpha1 to v1alpha2 in two phases with a " +
			"verification point between them.\n\n" +
			"Run preflight first. It reads the cluster, reports every check " +
			"and the plan, and changes nothing, so it is safe to run well " +
			"before the upgrade window.",
		SilenceUsage: true,
		// Cobra prints the error, and main does not. A failure is the one
		// thing a run must not swallow, and the reporter narrates progress
		// rather than the error a command returns, so silencing cobra here
		// would leave a non-zero exit with nothing said about it.
		SilenceErrors: false,
	}

	flags := root.PersistentFlags()
	flags.StringVar(&global.Kubeconfig, "kubeconfig", "",
		"path to a kubeconfig file, defaulting to the in-cluster configuration")
	flags.StringVar(&global.Context, "context", "", "the kubeconfig context to use")
	flags.StringVarP(&global.Namespace, "namespace", "n", defaultNamespace(), "the namespace the installation lives in")
	flags.StringSliceVar(&global.Skip, "skip", nil, "rule identities not to run; every skipped rule is reported")
	flags.BoolVar(&global.AcknowledgeOffline, "acknowledge-offline", false,
		"proceed even though a StorageNode is not online")
	flags.BoolVar(&global.Plain, "plain", false, "write line-by-line output even at a terminal")
	flags.BoolVarP(&global.Verbose, "verbose", "v", false, "report every rule as it runs")

	root.AddCommand(
		newPreflightCommand(&global),
		newUpgradeCommand(&global),
		newMigrateCommand(&global),
		newRulesCommand(&global),
	)
	return root
}

// defaultNamespace is where an installation lives unless told otherwise. It is
// read from the environment so the Job form of this tool needs no flag, and
// falls back to the namespace the chart installs into.
func defaultNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if ns := os.Getenv("SB_NAMESPACE"); ns != "" {
		return ns
	}
	return "simplyblock"
}

// signalContext is a context canceled by an interrupt, so a run stops between
// steps rather than in the middle of one. The framework's steps are each
// verified, and a run that stops between two of them resumes from the cluster.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// mustNamespace refuses a run with no namespace rather than defaulting to one,
// because the default would be somebody else's installation.
func mustNamespace(global *globalOptions) error {
	if global.Namespace == "" {
		return fmt.Errorf("no namespace: pass --namespace, or set POD_NAMESPACE")
	}
	return nil
}
