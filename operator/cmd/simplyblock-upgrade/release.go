// The release commands: an upgrade from the release a deployment runs to the
// release this binary belongs to, assembled from the generic sequence and the
// release modules the two releases select (design-release-upgrade.md §6, §7).
// They sit beside the API migration's commands and share their session, their
// reporter, and their flags.

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/release"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/releases"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/releases/registry"
)

// version is the release this binary belongs to, stamped at build time by
// setting main.version to the chart version through -ldflags. It is the target
// of a release upgrade unless --to says otherwise.
var version = "dev"

// releaseOptions are the release commands' own flags.
type releaseOptions struct {
	From, To         string
	SimplyblockImage string
	SpdkImage        string
	Settle           time.Duration
	Timeout          time.Duration
}

func newReleaseCommand(global *globalOptions) *cobra.Command {
	var opts releaseOptions
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Upgrade a deployment from the release it runs to this binary's release",
		Long: "release runs an upgrade between two releases of an installation that is already " +
			"on the v1alpha2 API. It selects what each release crossed needs, and runs it around " +
			"the generic sequence: the checks and the CRDs before the operator is upgraded " +
			"(prepare), and the control plane and the storage plane after it (rollout).",
	}
	flags := cmd.PersistentFlags()
	flags.StringVar(&opts.From, "from", "", "the release the deployment runs; read from its Helm release when unset")
	flags.StringVar(&opts.To, "to", "", "the release to upgrade to; this binary's own release when unset")
	flags.StringVar(&opts.SimplyblockImage, "simplyblock-image", "", "the control-plane image of the target release")
	flags.StringVar(&opts.SpdkImage, "spdk-image", "",
		"the SPDK image the target control plane reports as its default, which the storage plane waits for")
	flags.DurationVar(&opts.Settle, "settle", 2*time.Minute,
		"how long after the control-plane upgrade the clusters are given to report it, without --spdk-image")
	flags.DurationVar(&opts.Timeout, "timeout", 24*time.Hour, "how long one wait lasts before the run fails")

	cmd.AddCommand(
		releaseSubcommand(global, &opts, "plan", "Report the selected releases, the checks, and every step, changing nothing", runPlan),
		releaseSubcommand(global, &opts, "preflight", "Run the checks, changing nothing", runPreflight),
		releaseSubcommand(global, &opts, "prepare", "Run the checks, then the steps before the operator is upgraded", runPrepare),
		releaseSubcommand(global, &opts, "rollout", "Run the steps after the operator is upgraded", runRollout),
	)
	return cmd
}

type releaseRun func(ctx context.Context, s *session, selection releases.Selection) error

func releaseSubcommand(
	global *globalOptions, opts *releaseOptions, use, short string, run releaseRun,
) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			readOnly := dryRun || use == "plan" || use == "preflight"
			probe, err := newSessionWith(global, upgrade.StageReleasePrepare, true, upgrade.NewCatalog())
			if err != nil {
				return err
			}
			selection, err := selectReleases(ctx, probe, *opts)
			_ = probe.Close()
			if err != nil {
				return err
			}

			o := releases.Options{SimplyblockImage: opts.SimplyblockImage, SpdkImage: opts.SpdkImage,
				Settle: opts.Settle, Timeout: opts.Timeout}
			selection.Modules = filterModules(registry.Modules(o), selection)
			stage := upgrade.StageReleasePrepare
			if use == "rollout" {
				stage = upgrade.StageReleaseRollout
			}
			session, err := newSessionWith(global, stage, readOnly, releases.Build(selection, o))
			if err != nil {
				return err
			}
			defer session.Close() //nolint:errcheck // giving the terminal back cannot fail the run

			session.Reporter.Block("Release upgrade", []string{
				fmt.Sprintf("from %s to %s", selection.Source, selection.Target),
				"releases crossed that need something: " + listOrNone(selection.Releases()),
			})
			if selection.Source.Equal(selection.Target) {
				session.Reporter.Progress("the deployment already runs %s; there is nothing to do", selection.Target)
				return nil
			}
			return run(ctx, session, selection)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be done and write nothing")
	return cmd
}

// selectReleases reads the source and the target and checks the step between
// them, before anything else is built.
func selectReleases(ctx context.Context, s *session, opts releaseOptions) (releases.Selection, error) {
	targetText := opts.To
	if targetText == "" {
		targetText = version
	}
	if targetText == "dev" {
		return releases.Selection{}, fmt.Errorf("this binary was built without a release: pass --to")
	}
	target, err := releases.ParseVersion(targetText)
	if err != nil {
		return releases.Selection{}, err
	}

	sourceText := opts.From
	if sourceText == "" {
		deployed, found, err := release.Deployed(ctx, s.Client, s.Runner.Scope.Namespace)
		if err != nil {
			return releases.Selection{}, fmt.Errorf("read the deployed Helm release: %w", err)
		}
		if !found || deployed.ChartVersion == "" {
			return releases.Selection{}, fmt.Errorf("no Helm release of the operator in namespace %s: "+
				"pass --from with the release the deployment runs", s.Runner.Scope.Namespace)
		}
		sourceText = deployed.ChartVersion
	}
	source, err := releases.ParseVersion(sourceText)
	if err != nil {
		return releases.Selection{}, err
	}
	return releases.Select(registry.Modules(releases.Options{}), source, target)
}

// filterModules keeps the modules a selection chose, configured with the run's
// options, which the selection itself was made without.
func filterModules(all []releases.Module, selection releases.Selection) []releases.Module {
	var out []releases.Module
	for _, m := range all {
		for _, chosen := range selection.Modules {
			if m.Release.Equal(chosen.Release) {
				out = append(out, m)
			}
		}
	}
	return out
}

func runPlan(ctx context.Context, s *session, _ releases.Selection) error {
	findings, err := s.Runner.Check(ctx, upgrade.StageReleasePrepare)
	if err != nil {
		return err
	}
	for _, stage := range []upgrade.Stage{upgrade.StageReleasePrepare, upgrade.StageReleaseRollout} {
		tasks, err := s.Runner.Tasks(ctx, stage)
		if err != nil {
			return err
		}
		plan := upgrade.Plan{Stage: stage, Tasks: tasks}
		if stage == upgrade.StageReleasePrepare {
			plan.Findings = findings
		}
		s.Reporter.Plan(plan)
	}
	return nil
}

func runPreflight(ctx context.Context, s *session, _ releases.Selection) error {
	findings, err := s.Runner.Check(ctx, upgrade.StageReleasePrepare)
	if err != nil {
		return err
	}
	s.Reporter.Findings(findings)
	if findings.Blocked() {
		return fmt.Errorf("preflight failed: %d violations, and no changes were made", len(findings.Errors()))
	}
	return nil
}

func runPrepare(ctx context.Context, s *session, selection releases.Selection) error {
	if err := runPreflight(ctx, s, selection); err != nil {
		return err
	}
	return s.Runner.ApplyAll(ctx, upgrade.StageReleasePrepare)
}

func runRollout(ctx context.Context, s *session, _ releases.Selection) error {
	return s.Runner.ApplyAll(ctx, upgrade.StageReleaseRollout)
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
