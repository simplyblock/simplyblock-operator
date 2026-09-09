// The plan: what a stage would do, computed by asking every step to describe
// its work without performing it. §27 of the design puts the plan in the
// read-only command, so the walk that reports it is the same walk that would
// perform it, with the writes left out.

package upgrade

import (
	"fmt"
	"sort"
	"strings"
)

// Verb is what an action does to an object. The set is closed on purpose: a
// step that cannot describe its work as one of these is doing something the
// plan cannot show a user, which is the thing the plan exists to prevent.
type Verb string

const (
	// VerbCreate makes an object that does not exist.
	VerbCreate Verb = "CREATE"

	// VerbUpdate changes an object's spec or data in place.
	VerbUpdate Verb = "UPDATE"

	// VerbDelete removes an object.
	VerbDelete Verb = "DELETE"

	// VerbReparent moves an object's controller reference to a new owner. It is
	// distinct from an update because it is the one change that decides what
	// garbage collection removes.
	VerbReparent Verb = "REPARENT"

	// VerbAnnotate writes metadata and nothing else: the release handover's
	// resource policy, and the normalized volume handle on an object whose
	// field is immutable.
	VerbAnnotate Verb = "ANNOTATE"

	// VerbRewrite writes an object back unchanged, which is what moves it
	// between storage representations.
	VerbRewrite Verb = "REWRITE"
)

// Action is one change a step intends to make to one object.
type Action struct {
	// Rule is the step that would perform it.
	Rule ID

	// Verb is what happens to the object.
	Verb Verb

	// Object is what it happens to.
	Object ObjectRef

	// Detail says what changes, in the form the plan prints: an old value, an
	// arrow, and a new one.
	Detail string
}

// String renders one line of the plan.
func (a Action) String() string {
	if a.Detail == "" {
		return fmt.Sprintf("%-9s %s", a.Verb, a.Object)
	}
	return fmt.Sprintf("%-9s %s\n            %s", a.Verb, a.Object, a.Detail)
}

// Plan is everything a stage would do, and everything its checks found. Both
// halves are here because §27 has the one read-only command report both, and
// because a plan whose checks failed is a plan that will not run.
type Plan struct {
	// Stage is the command the plan describes.
	Stage Stage

	// Actions are the changes, in the order the steps would perform them.
	Actions []Action

	// Findings are what the checks reported.
	Findings Findings

	// Skipped names the rules that did not run because the command line named
	// them, so the report can say what was not checked.
	Skipped []ID
}

// Add appends actions.
func (p *Plan) Add(actions ...Action) { p.Actions = append(p.Actions, actions...) }

// Record appends findings.
func (p *Plan) Record(findings ...Finding) { p.Findings = append(p.Findings, findings...) }

// Blocked reports whether the plan's checks refuse the stage.
func (p *Plan) Blocked() bool { return p.Findings.Blocked() }

// Summary renders the counts §27 closes the plan with, one line per verb and
// kind, sorted so two runs over one cluster print the same report.
func (p *Plan) Summary() []string {
	type bucket struct {
		verb Verb
		kind string
	}
	counts := make(map[bucket]int)
	for _, action := range p.Actions {
		kind := action.Object.GVK.Kind
		if kind == "" {
			kind = "object"
		}
		counts[bucket{action.Verb, kind}]++
	}

	keys := make([]bucket, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].verb != keys[j].verb {
			return keys[i].verb < keys[j].verb
		}
		return keys[i].kind < keys[j].kind
	})

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, fmt.Sprintf("%d %s will be %s.", counts[key], plural(key.kind, counts[key]), pastTense(key.verb)))
	}
	return out
}

// plural is enough English for the summary lines, which name Kubernetes kinds
// and nothing else.
func plural(kind string, n int) string {
	if n == 1 {
		return kind
	}
	switch {
	case strings.HasSuffix(kind, "s"), strings.HasSuffix(kind, "x"), strings.HasSuffix(kind, "ch"):
		return kind + "es"
	default:
		return kind + "s"
	}
}

// pastTense renders a verb the way the summary reads it, as in one
// StorageNodeSet will be deleted.
func pastTense(v Verb) string {
	switch v {
	case VerbCreate:
		return "created"
	case VerbUpdate:
		return "updated"
	case VerbDelete:
		return "deleted"
	case VerbReparent:
		return "reparented"
	case VerbAnnotate:
		return "annotated"
	case VerbRewrite:
		return "rewritten"
	default:
		return strings.ToLower(string(v))
	}
}
