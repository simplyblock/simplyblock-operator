// What a check reports. A finding is the unit both the preflight's report and
// the migration's refusal are built out of, so it carries everything a user
// needs to act without reading the code that produced it: the object, the value
// that was wrong, the limit it broke, and what to do about it.

package upgrade

import (
	"fmt"
	"strings"
)

// Severity is what a finding does to the run.
type Severity string

const (
	// SeverityError blocks. A stage that produced one makes no changes, and one
	// produced mid-stage stops before the next step.
	SeverityError Severity = "ERROR"

	// SeverityWarning is reported and does not block. It is for a condition the
	// user should see and the migration can carry, such as a derived name the
	// migration is about to rewrite.
	SeverityWarning Severity = "WARNING"

	// SeverityInfo is reported and means nothing is wrong. It is how a check
	// records what it examined on a cluster where everything passed.
	SeverityInfo Severity = "INFO"
)

// Finding is one thing a check has to say about one object.
type Finding struct {
	// Rule is the check that produced it.
	Rule ID

	// Severity decides whether the run continues.
	Severity Severity

	// Objects are what the finding is about. It is a list because the
	// interesting findings are about pairs: two pools that derive one
	// StorageClass name, or two same-named objects in two namespaces that
	// become one object of a kind that is becoming cluster-scoped.
	Objects []ObjectRef

	// PerObject is a note rendered beside the object at the same index, for a
	// finding whose objects each contributed something different. A collision
	// is the case that needs it, since what a reader wants next after the two
	// objects is what each of them derived its half from, and repeating the
	// object names in Detail to say so prints every one of them twice.
	//
	// It is either empty or as long as Objects. A shorter one leaves the
	// remaining objects unannotated rather than misaligned.
	PerObject []string

	// Summary is the one line a report prints first.
	Summary string

	// Detail is what the check computed: the derived value, its length, and the
	// limit that bound it. Rendered under the summary, indented.
	Detail string

	// Remediation is what the user does about it, in the imperative. A finding
	// with no remediation is one the framework could not tell the user how to
	// fix, which is a gap in the check rather than in the cluster.
	Remediation string
}

// Error reports whether the finding blocks.
func (f Finding) Error() bool { return f.Severity == SeverityError }

// String renders the finding as the preflight prints it.
func (f Finding) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-7s %s", f.Severity, f.Summary)
	for i, obj := range f.Objects {
		if i < len(f.PerObject) && f.PerObject[i] != "" {
			fmt.Fprintf(&b, "\n        %s  %s", obj, f.PerObject[i])
			continue
		}
		fmt.Fprintf(&b, "\n        %s", obj)
	}
	for _, line := range nonEmptyLines(f.Detail) {
		fmt.Fprintf(&b, "\n        %s", line)
	}
	for _, line := range nonEmptyLines(f.Remediation) {
		fmt.Fprintf(&b, "\n        fix: %s", line)
	}
	return b.String()
}

// Findings is a set of findings, and the questions a runner asks of one.
type Findings []Finding

// Errors returns only the blocking findings.
func (f Findings) Errors() Findings {
	var out Findings
	for _, finding := range f {
		if finding.Error() {
			out = append(out, finding)
		}
	}
	return out
}

// Blocked reports whether anything in the set stops the run.
func (f Findings) Blocked() bool { return len(f.Errors()) > 0 }

// String renders every finding, one after another. It is what a report prints
// and what a test compares two runs with.
func (f Findings) String() string {
	rendered := make([]string, 0, len(f))
	for _, finding := range f {
		rendered = append(rendered, finding.String())
	}
	return strings.Join(rendered, "\n")
}

// Count reports how many findings carry this severity.
func (f Findings) Count(severity Severity) int {
	n := 0
	for _, finding := range f {
		if finding.Severity == severity {
			n++
		}
	}
	return n
}

// nonEmptyLines splits a multi-line detail into the lines a report indents,
// dropping the blank ones a heredoc leaves behind.
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimRight(line, " \t"); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
