// Where a run says what it is doing. Output is an interface rather than a set
// of print statements for two reasons. §25 requires that a migration which is
// correctly refusing to proceed and one that has hung are told apart, which
// means every held decision is announced somewhere a user is looking. And the
// tool runs both at a terminal and as a Job in the cluster, where a progress
// bar redrawing itself is a log file nobody can read.
//
// So the runner announces work and outcomes, and an implementation decides what
// that looks like: [TextReporter] writes lines, and upgrade/tui draws a
// progress bar over the same events.

package upgrade

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// Outcome is how a rule finished. It is what advances a progress bar and what
// decides the mark a line is printed with.
type Outcome string

const (
	// OutcomeDone means the rule ran and reported nothing wrong.
	OutcomeDone Outcome = "done"

	// OutcomeSkipped means the rule did not run: its effect was already
	// present, or the command line excluded it.
	OutcomeSkipped Outcome = "skipped"

	// OutcomeFailed means the rule refused, or could not be performed.
	OutcomeFailed Outcome = "failed"
)

// Reporter receives everything a run has to say. An implementation renders it,
// and none of them decides anything.
//
// A reporter must be safe for concurrent use. Discovery of several kinds runs
// in parallel, and a terminal implementation renders from a goroutine of its
// own.
type Reporter interface {
	// Stage announces the command that started and how many steps it holds, so
	// a reporter can size a progress bar rather than guess at one. A steps
	// count of zero means the stage's work is not countable in advance, which
	// is what the preflight is.
	Stage(stage Stage, steps int)

	// Phase announces a migrate state being entered, and how many steps are
	// registered in it (§23).
	Phase(phase Phase, steps int)

	// Rule announces that a rule is about to run.
	Rule(rule Rule)

	// Outcome announces how it finished. The detail says why, for a rule that
	// was skipped or refused, and is empty otherwise.
	Outcome(rule Rule, outcome Outcome, detail string)

	// Findings reports what a check found.
	Findings(findings Findings)

	// Action reports a change that was made, or that would be made under a dry
	// run.
	Action(action Action)

	// Plan renders a whole plan.
	Plan(plan Plan)

	// Progress is a line of narration: the counts of a paced rewrite, or the
	// wait a step is holding on.
	Progress(format string, args ...any)

	// Close releases whatever the reporter holds, which for a terminal
	// implementation is the terminal. It is safe to call more than once.
	Close() error
}

// DiscardReporter throws everything away. It is the default a [Scope] takes
// when none was supplied, so a rule never has to guard against a nil reporter.
type DiscardReporter struct{}

func (DiscardReporter) Stage(Stage, int)              {}
func (DiscardReporter) Phase(Phase, int)              {}
func (DiscardReporter) Rule(Rule)                     {}
func (DiscardReporter) Outcome(Rule, Outcome, string) {}
func (DiscardReporter) Findings(Findings)             {}
func (DiscardReporter) Action(Action)                 {}
func (DiscardReporter) Plan(Plan)                     {}
func (DiscardReporter) Progress(string, ...any)       {}
func (DiscardReporter) Close() error                  { return nil }

// TextReporter writes the report a person reads in a terminal that is not one,
// which is a log file, a CI job, and the Job this tool runs as in a cluster. It
// is the shape §19.11 and §27 print.
type TextReporter struct {
	// Out is where the report goes.
	Out io.Writer

	// Verbose prints every rule as it starts, rather than only the ones with
	// something to say. It is what a user turns on when a run is taking longer
	// than they expected.
	Verbose bool

	mu sync.Mutex
}

// NewTextReporter builds a reporter over a writer.
func NewTextReporter(out io.Writer) *TextReporter { return &TextReporter{Out: out} }

func (r *TextReporter) Stage(stage Stage, steps int) {
	r.line("")
	if steps > 0 {
		r.line("── %s, %d steps ──────────────────────────────", stage, steps)
		return
	}
	r.line("── %s ────────────────────────────────────────", stage)
}

func (r *TextReporter) Phase(phase Phase, steps int) {
	r.line("")
	r.line("  %s (%d steps)", phase, steps)
}

func (r *TextReporter) Rule(rule Rule) {
	if r.Verbose {
		r.line("  · %s: %s", rule.ID(), rule.Description())
	}
}

func (r *TextReporter) Outcome(rule Rule, outcome Outcome, detail string) {
	switch {
	case outcome == OutcomeDone && !r.Verbose:
		return
	case detail == "":
		r.line("  %s %s", mark(outcome), rule.ID())
	default:
		r.line("  %s %s: %s", mark(outcome), rule.ID(), detail)
	}
}

func (r *TextReporter) Findings(findings Findings) {
	for _, finding := range findings {
		r.line("%s", finding)
		r.line("")
	}
}

func (r *TextReporter) Action(action Action) {
	r.line("  %s", action)
}

func (r *TextReporter) Plan(plan Plan) {
	for _, action := range plan.Actions {
		r.Action(action)
	}
	r.Findings(plan.Findings)

	r.line("")
	for _, line := range plan.Summary() {
		r.line("%s", line)
	}
	if errs := plan.Findings.Errors(); len(errs) > 0 {
		r.line("")
		r.line("%s failed: %d violations. No changes were made.", plan.Stage, len(errs))
	}
}

func (r *TextReporter) Progress(format string, args ...any) {
	r.line("  "+format, args...)
}

// Close writes nothing. A text reporter holds no terminal.
func (r *TextReporter) Close() error { return nil }

// line writes one line under the reporter's lock, so two goroutines cannot
// interleave halves of a finding.
func (r *TextReporter) line(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	text := fmt.Sprintf(format, args...)
	// A report that cannot be written is not a reason to fail a migration, and
	// the run's own error is what a caller acts on.
	_, _ = fmt.Fprintln(r.Out, strings.TrimRight(text, " "))
}

// mark is the character an outcome is printed with, chosen so a log stays
// readable where the terminal has no color.
func mark(outcome Outcome) string {
	switch outcome {
	case OutcomeSkipped:
		return "~"
	case OutcomeFailed:
		return "✗"
	default:
		return "✓"
	}
}
