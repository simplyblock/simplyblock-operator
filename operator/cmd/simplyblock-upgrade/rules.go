// The command that reports what the tool knows how to do. It exists because the
// catalog is the extension point: a rule added to the product is a rule this
// command lists, and a user reading a report that names a rule can find out
// what that rule is without reading the code.

package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/catalog"
)

// newRulesCommand builds `simplyblock-upgrade rules`. It contacts no cluster.
func newRulesCommand(_ *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "rules",
		Short: "List the checks, steps, and transformations this build carries",
		Long: "rules prints the catalog: every discoverer, check, naming rule, " +
			"step, and transformation, with the identity a report names it by " +
			"and the identity --skip takes.\n\n" +
			"It contacts no cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := catalog.Default()
			out := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

			section(out, "DISCOVERERS", c.Discoverers.All())
			section(out, "CHECKS", withStages(c.Checks.All()))
			section(out, "NAMING RULES", c.Derivations.All())
			section(out, "STEPS", withPhases(c.Steps.All()))
			section(out, "TRANSFORMATIONS", c.Transformations.All())

			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintf(out, "migrate walks: %s\n", strings.Join(phaseNames(), " → "))
			return out.Flush()
		},
	}
}

// annotated is a rule with the extra column its kind carries: the stages a
// check runs in, the phase a step belongs to.
type annotated struct {
	rule  upgrade.Rule
	extra string
}

// section prints one part of the catalog, or says it is empty. The write errors
// are dropped because a tabwriter defers them to Flush, which the caller
// returns.
func section[T any](out *tabwriter.Writer, title string, rules []T) {
	_, _ = fmt.Fprintf(out, "\n%s\n", title)
	if len(rules) == 0 {
		_, _ = fmt.Fprintf(out, "  (none registered in this build)\n")
		return
	}
	for _, entry := range rules {
		switch rule := any(entry).(type) {
		case annotated:
			_, _ = fmt.Fprintf(out, "  %s\t%s\t%s\n", rule.rule.ID(), rule.extra, rule.rule.Description())
		case upgrade.Rule:
			_, _ = fmt.Fprintf(out, "  %s\t\t%s\n", rule.ID(), rule.Description())
		}
	}
}

// withStages annotates checks with the commands they run in.
func withStages(checks []upgrade.Check) []annotated {
	out := make([]annotated, 0, len(checks))
	for _, check := range checks {
		stages := make([]string, 0, len(check.Stages()))
		for _, stage := range check.Stages() {
			stages = append(stages, string(stage))
		}
		out = append(out, annotated{rule: check, extra: strings.Join(stages, ",")})
	}
	return out
}

// withPhases annotates steps with the stage and, for the migration, the phase.
func withPhases(steps []upgrade.Step) []annotated {
	out := make([]annotated, 0, len(steps))
	for _, step := range steps {
		extra := string(step.Stage())
		if step.Stage() == upgrade.StageMigrate {
			extra += "/" + string(step.Phase())
		}
		out = append(out, annotated{rule: step, extra: extra})
	}
	return out
}

// phaseNames is the migrate walk, printed so the phase a report names has a
// place in a sequence the user can see.
func phaseNames() []string {
	out := make([]string, 0, len(upgrade.MigratePhases))
	for _, phase := range upgrade.MigratePhases {
		out = append(out, string(phase))
	}
	return out
}
