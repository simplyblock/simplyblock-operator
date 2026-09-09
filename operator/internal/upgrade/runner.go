// The runner: what turns a catalog into a run. It owns every decision the
// design makes about control flow, so that a rule stays a description of one
// thing and none of them can change what a failure means.
//
// The decisions, all in one place:
//
//   - Discovery runs first and completely. A check reading a half-built graph
//     draws a wrong conclusion from an object that is merely absent (§17).
//   - A blocking finding stops the stage before any step runs, and stops a
//     migrate phase before the next one is entered (§18, §25).
//   - A step is asked whether it is done before it is applied, and verified
//     after (§21, §22).
//   - A dry run plans and never applies, which is what the preflight is (§27).

package upgrade

import (
	"context"
	"fmt"
)

// Runner executes a stage against a cluster.
type Runner struct {
	// Catalog is the set of rules the run is assembled from.
	Catalog *Catalog

	// Scope is the cluster and the run's switches.
	Scope *Scope
}

// NewRunner builds a runner.
func NewRunner(catalog *Catalog, scope *Scope) *Runner {
	return &Runner{Catalog: catalog, Scope: scope}
}

// Discover builds the graph, running each discoverer after the ones it
// requires. It is called by every stage, because none of them can validate or
// change a cluster it has not read.
func (r *Runner) Discover(ctx context.Context) error {
	discoverers, err := orderDiscoverers(r.Catalog.Discoverers.Select(func(d Discoverer) bool {
		return !r.Scope.Options.Skipped(d.ID())
	}))
	if err != nil {
		return err
	}

	for _, discoverer := range discoverers {
		r.Scope.Report.Rule(discoverer)
		if err := discoverer.Discover(ctx, r.Scope); err != nil {
			r.Scope.Report.Outcome(discoverer, OutcomeFailed, err.Error())
			return fmt.Errorf("discovery %q: %w", discoverer.ID(), err)
		}
		r.Scope.Report.Outcome(discoverer, OutcomeDone, "")
	}
	r.Scope.Report.Progress("discovered %d objects across %d kinds",
		r.Scope.Graph.Len(), len(r.Scope.Graph.Kinds()))
	return nil
}

// Check runs the stage's checks and returns everything they found. It reports
// an error only when a check could not be performed, which is a different
// outcome from a finding: a check that could not read the cluster has proven
// nothing about it, and the run stops whatever the severities say.
func (r *Runner) Check(ctx context.Context, stage Stage) (Findings, error) {
	var findings Findings
	for _, check := range r.Catalog.ChecksFor(stage, r.Scope.Options) {
		r.Scope.Report.Rule(check)

		found, err := check.Check(ctx, r.Scope)
		if err != nil {
			r.Scope.Report.Outcome(check, OutcomeFailed, err.Error())
			return findings, fmt.Errorf("check %q could not be performed: %w", check.ID(), err)
		}
		findings = append(findings, found...)

		outcome := OutcomeDone
		if found.Blocked() {
			outcome = OutcomeFailed
		}
		r.Scope.Report.Outcome(check, outcome, "")
	}

	for _, skipped := range r.skippedChecks(stage) {
		r.Scope.Report.Outcome(skipped, OutcomeSkipped, "named on the command line")
	}
	return findings, nil
}

// skippedChecks lists the stage's checks the command line excluded, so the
// report can say what was not checked rather than implying everything was.
func (r *Runner) skippedChecks(stage Stage) []Check {
	return r.Catalog.Checks.Select(func(check Check) bool {
		return RunsIn(check, stage) && r.Scope.Options.Skipped(check.ID())
	})
}

// Plan asks every rule of a stage what it would do, and changes nothing. It is
// the whole of the preflight and the first thing the other two stages report.
func (r *Runner) Plan(ctx context.Context, stage Stage) (Plan, error) {
	plan := Plan{Stage: stage}

	findings, err := r.Check(ctx, stage)
	plan.Record(findings...)
	if err != nil {
		return plan, err
	}
	for _, skipped := range r.skippedChecks(stage) {
		plan.Skipped = append(plan.Skipped, skipped.ID())
	}

	steps, err := r.Catalog.StepsFor(stage, r.Scope.Options)
	if err != nil {
		return plan, err
	}
	for _, step := range steps {
		actions, err := step.Plan(ctx, r.Scope)
		if err != nil {
			return plan, fmt.Errorf("step %q could not be planned: %w", step.ID(), err)
		}
		plan.Add(actions...)
	}
	return plan, nil
}

// Apply runs one step: skipping it when its effect is already present,
// performing it otherwise, and verifying it either way.
//
// Verification runs on a step that reported itself done as well as on one that
// was just applied. A rerun that trusts Done and skips the check is a rerun
// that cannot notice that the state it is resuming from is not the state it
// thinks, which is the failure §25 refuses to let pass silently.
func (r *Runner) Apply(ctx context.Context, step Step) error {
	r.Scope.Report.Rule(step)

	done, err := step.Done(ctx, r.Scope)
	if err != nil {
		return fmt.Errorf("step %q could not report whether it had run: %w", step.ID(), err)
	}

	switch {
	case done:
		r.Scope.Report.Outcome(step, OutcomeSkipped, "already performed")
	case r.Scope.Options.DryRun:
		actions, err := step.Plan(ctx, r.Scope)
		if err != nil {
			return fmt.Errorf("step %q could not be planned: %w", step.ID(), err)
		}
		for _, action := range actions {
			r.Scope.Report.Action(action)
		}
		// A dry run performed nothing, so there is nothing to verify and
		// asserting the post-conditions would fail on every one of them.
		r.Scope.Report.Outcome(step, OutcomeSkipped, "dry run")
		return nil
	default:
		if err := step.Apply(ctx, r.Scope); err != nil {
			r.Scope.Report.Outcome(step, OutcomeFailed, err.Error())
			return fmt.Errorf("step %q: %w", step.ID(), err)
		}
	}

	for _, verification := range step.Verifications() {
		if err := verification.Verify(ctx, r.Scope); err != nil {
			r.Scope.Report.Outcome(step, OutcomeFailed, err.Error())
			return fmt.Errorf("step %q was not verified by %q: %w", step.ID(), verification.ID(), err)
		}
	}
	if !done {
		r.Scope.Report.Outcome(step, OutcomeDone, "")
	}
	return nil
}

// ApplyAll runs a stage's steps in dependency order, stopping at the first that
// fails or cannot be verified.
func (r *Runner) ApplyAll(ctx context.Context, stage Stage) error {
	steps, err := r.Catalog.StepsFor(stage, r.Scope.Options)
	if err != nil {
		return err
	}
	r.Scope.Report.Stage(stage, len(steps))
	for _, step := range steps {
		if err := r.Apply(ctx, step); err != nil {
			return err
		}
	}
	return nil
}
