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
	"strings"
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

	r.Scope.Report.Section("Collecting simplyblock Kubernetes resources", len(discoverers))

	for _, discoverer := range discoverers {
		r.Scope.Report.Rule(discoverer)

		// The count before and after is what a discoverer reports having
		// found. Asking the graph is cheaper than having every discoverer
		// return a number it would only be used to print.
		before := r.Scope.Graph.Len()
		if err := discoverer.Discover(ctx, r.Scope); err != nil {
			r.Scope.Report.Outcome(discoverer, OutcomeFailed, err.Error())
			return fmt.Errorf("discovery %q: %w", discoverer.ID(), err)
		}
		found := r.Scope.Graph.Len() - before
		r.Scope.Report.Outcome(discoverer, OutcomeDone, objectCount(found))
	}

	r.Scope.Report.Progress("%d objects across %d kinds in %d namespace(s)",
		r.Scope.Graph.Len(), len(r.Scope.Graph.Kinds()), len(r.Scope.Occupied()))
	return nil
}

// objectCount is how a discoverer's result reads in a report. A kind with no
// objects says so rather than saying nothing, because on a cluster where a
// check found nothing the next question is always whether anything was read.
func objectCount(n int) string {
	switch n {
	case 0:
		return "no objects"
	case 1:
		return "1 object"
	default:
		return fmt.Sprintf("%d objects", n)
	}
}

// Check runs the stage's checks and returns everything they found. It reports
// an error only when a check could not be performed, which is a different
// outcome from a finding: a check that could not read the cluster has proven
// nothing about it, and the run stops whatever the severities say.
func (r *Runner) Check(ctx context.Context, stage Stage) (Findings, error) {
	checks := r.Catalog.ChecksFor(stage, r.Scope.Options)
	r.Scope.Report.Section("Verifying resources", len(checks))

	var findings Findings
	for _, check := range checks {
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

// Tasks reports what one stage's steps would do, without running its checks.
//
// It is separate from [Runner.Plan] because the preflight reports both stages
// and the checks belong to the run rather than to either of them: running them
// once per stage would print every finding twice.
func (r *Runner) Tasks(ctx context.Context, stage Stage) ([]Task, error) {
	steps, err := r.Catalog.StepsFor(stage, r.Scope.Options)
	if err != nil {
		return nil, err
	}

	tasks := make([]Task, 0, len(steps))
	for _, step := range steps {
		task, err := r.planStep(ctx, step)
		if err != nil {
			return nil, err
		}
		if len(task.Subtasks) > 0 {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
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

	tasks, err := r.Tasks(ctx, stage)
	if err != nil {
		return plan, err
	}
	plan.Add(tasks...)
	return plan, nil
}

// planStep reports what one step would do, which is exactly what it describes.
//
// There is no second question here. A step describes nothing for a subject that
// is already in the state it exists to produce, so a plan taken after a partial
// run shows what is left rather than what was originally intended.
func (r *Runner) planStep(ctx context.Context, step Step) (Task, error) {
	task := Task{
		Step:    step.ID(),
		Summary: step.Description(),
		Phase:   step.Phase(),
		Blocked: blockedBy(step),
	}

	subjects, err := SubjectsFor(ctx, r.Scope, step)
	if err != nil {
		return task, err
	}
	for _, subject := range subjects {
		action, err := step.Describe(ctx, r.Scope, subject)
		if err != nil {
			return task, fmt.Errorf("step %q could not describe %s: %w", step.ID(), subject, err)
		}
		if action != nil {
			task.Subtasks = append(task.Subtasks, *action)
		}
	}
	return task, nil
}

// Apply runs one step over every subject: performing the change it describes,
// and doing nothing where it describes none.
//
// A subject the step declines is not validated, applied, or verified. There is
// nothing to confirm there, and the inspection that would confirm it is the one
// Describe already made.
//
// A failed precondition stops the step there rather than partway through the
// next subject, because §25 requires that nothing downstream depend on an
// unverified change.
func (r *Runner) Apply(ctx context.Context, step Step) error {
	r.Scope.Report.Rule(step)

	subjects, err := SubjectsFor(ctx, r.Scope, step)
	if err != nil {
		return r.stepFailed(step, err)
	}
	r.Scope.Report.Work(len(subjects))

	changed, finished := 0, 0
	for _, subject := range subjects {
		action, err := step.Describe(ctx, r.Scope, subject)
		if err != nil {
			return r.stepFailed(step, fmt.Errorf("describing %s: %w", subject, err))
		}
		if action == nil {
			// Nothing to do. Done tells the two reasons apart, which is what
			// the outcome line reports and what a coverage check reads.
			done, err := step.Done(ctx, r.Scope, subject)
			if err != nil {
				return r.stepFailed(step, fmt.Errorf("asking whether %s was finished: %w", subject, err))
			}
			if done {
				finished++
			}
			continue
		}

		if r.Scope.Options.DryRun {
			r.Scope.Report.Action(*action)
			continue
		}

		if err := step.Validate(ctx, r.Scope, subject); err != nil {
			return r.stepFailed(step, fmt.Errorf("%s is not ready for this change: %w", subject, err))
		}
		r.Scope.Report.Item(action.String())
		if err := step.Apply(ctx, r.Scope, subject); err != nil {
			return r.stepFailed(step, fmt.Errorf("changing %s: %w", subject, err))
		}
		if err := step.Verify(ctx, r.Scope, subject); err != nil {
			return r.stepFailed(step, fmt.Errorf("%s was not verified: %w", subject, err))
		}
		changed++
	}

	switch {
	case r.Scope.Options.DryRun:
		r.Scope.Report.Outcome(step, OutcomeSkipped, "dry run")
	case changed > 0:
		r.Scope.Report.Outcome(step, OutcomeDone, objectCount(changed)+" changed")
	case finished > 0:
		r.Scope.Report.Outcome(step, OutcomeSkipped, "already performed")
	default:
		r.Scope.Report.Outcome(step, OutcomeSkipped, "nothing to do")
	}
	return nil
}

// refuseIfIncomplete reports the steps this build describes and cannot perform.
func refuseIfIncomplete(steps []Step) error {
	var missing []string
	for _, step := range steps {
		if reason := blockedBy(step); reason != "" {
			missing = append(missing, fmt.Sprintf("  %s: %s", step.ID(), reason))
		}
	}
	if len(missing) == 0 {
		return nil
	}

	return fmt.Errorf(
		"this build describes %d step(s) it cannot perform, and a stage it cannot "+
			"complete is one it must not start:\n%s",
		len(missing), strings.Join(missing, "\n"))
}

// stepFailed reports the failure and wraps it with the step that produced it.
func (r *Runner) stepFailed(step Step, err error) error {
	r.Scope.Report.Outcome(step, OutcomeFailed, err.Error())
	return fmt.Errorf("step %q: %w", step.ID(), err)
}

// ApplyAll runs a stage's steps in dependency order, stopping at the first that
// fails or cannot be verified.
func (r *Runner) ApplyAll(ctx context.Context, stage Stage) error {
	steps, err := r.Catalog.StepsFor(stage, r.Scope.Options)
	if err != nil {
		return err
	}

	// Before anything is applied. A stage that performed its first four steps
	// and stopped at the fifth would leave the cluster halfway through an
	// upgrade nothing can finish, so a stage this build cannot complete is one
	// it does not start.
	if err := refuseIfIncomplete(steps); err != nil {
		return err
	}

	r.Scope.Report.Section(stage.Describe(), len(steps))
	for _, step := range steps {
		if err := r.Apply(ctx, step); err != nil {
			return err
		}
	}
	return nil
}
