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

	// VerbAwait waits for a condition the upgrade cannot proceed without: TLS
	// material on disk, a Pod ready, a Service with endpoints, a CRD
	// established. It changes nothing and it is where an upgrade spends most
	// of its time, so a plan that left it out would not describe the wait a
	// user is about to sit through.
	VerbAwait Verb = "AWAIT"

	// VerbVerify confirms something without changing it. §9.1 numbers three of
	// these separately from the changes they confirm, because each is a
	// distinct thing that can fail.
	VerbVerify Verb = "VERIFY"
)

// Action is one change a step intends to make to one object.
type Action struct {
	// Rule is the step that would perform it.
	Rule ID

	// Verb is what happens to the object.
	Verb Verb

	// Object is what it happens to.
	Object ObjectRef

	// Detail is the change itself, in the form the plan prints: an old value,
	// an arrow, and a new one.
	//
	// It is the data, not a description of the step. A step's own explanation
	// belongs in its source, where it is read once, rather than in a line
	// printed on every run of a command whose job is to say what will happen
	// to which object. A step with nothing concrete to say leaves it empty.
	Detail string

	// Blocked marks an action this build describes and cannot perform. It is
	// set by the runner from the step, so a plan shows the whole of what a
	// stage owes and marks the part this build cannot do.
	//
	// What it holds is not printed. Why a step is unimplemented is a fact
	// about this build rather than about the cluster, so it belongs in a TODO
	// beside the step and in the error the stage refuses with, and the plan
	// says only that the line is one of them.
	Blocked string
}

// String renders one subtask.
func (a Action) String() string {
	line := fmt.Sprintf("%-9s %s", a.Verb, a.Object)
	if a.Detail != "" {
		line += "  " + a.Detail
	}
	return line
}

// Task is one step's work: what the step is, and the subjects it acts on.
//
// A plan is a hierarchy because the execution is one. A step acts on many
// objects, and a flat list of changes loses which step is responsible for each
// and how many objects one step touches, which is the difference between
// "annotate the release's survivors" and the ninety-five annotations that is.
type Task struct {
	// Step is the step whose work this is, and is what --skip takes.
	Step ID

	// Summary is the step's own description.
	Summary string

	// Phase groups the task, for the migration. It is empty for a stage whose
	// steps are a sequence rather than a graph.
	Phase Phase

	// Blocked marks a task this build describes and cannot perform.
	Blocked string

	// Subtasks are the changes, one per subject the step is about.
	Subtasks []Action
}

// Collapsed reports a task whose subtasks say nothing the task line does not.
//
// A step acting on the upgrade itself has exactly one subject and it carries no
// information, so printing it under the step repeats the step. A step whose
// subjects are objects prints them, because which objects and how many is the
// whole of what a plan is for.
func (t Task) Collapsed() bool {
	if len(t.Subtasks) != 1 {
		return false
	}
	return t.Subtasks[0].Object.GVK.Kind == UpgradeKind
}

// Plan is everything a stage would do, and everything its checks found. Both
// halves are here because §27 has the one read-only command report both, and
// because a plan whose checks failed is a plan that will not run.
type Plan struct {
	// Stage is the command the plan describes.
	Stage Stage

	// Tasks are the steps, in the order they would run.
	Tasks []Task

	// Findings are what the checks reported.
	Findings Findings

	// Skipped names the rules that did not run because the command line named
	// them, so the report can say what was not checked.
	Skipped []ID
}

// Add appends a task, dropping one with nothing to do.
func (p *Plan) Add(tasks ...Task) {
	for _, task := range tasks {
		if len(task.Subtasks) == 0 {
			continue
		}
		p.Tasks = append(p.Tasks, task)
	}
}

// Actions is every change the plan holds, flattened, which is what the counts
// are taken over.
func (p *Plan) Actions() []Action {
	var out []Action
	for _, task := range p.Tasks {
		out = append(out, task.Subtasks...)
	}
	return out
}

// Phases returns the phases the plan's tasks fall into, in the order the
// migration walks them, and a single empty phase for a stage that has none.
func (p *Plan) Phases() []Phase {
	seen := make(map[Phase]bool, len(p.Tasks))
	for _, task := range p.Tasks {
		seen[task.Phase] = true
	}

	var out []Phase
	if seen[""] {
		out = append(out, "")
	}
	for _, phase := range MigratePhases {
		if seen[phase] {
			out = append(out, phase)
		}
	}
	return out
}

// InPhase returns the tasks of one phase, in plan order.
func (p *Plan) InPhase(phase Phase) []Task {
	var out []Task
	for _, task := range p.Tasks {
		if task.Phase == phase {
			out = append(out, task)
		}
	}
	return out
}

// Record appends findings.
func (p *Plan) Record(findings ...Finding) {
	p.Findings = append(p.Findings, findings...)
}

// Blocked reports whether the plan's checks refuse the stage.
func (p *Plan) Blocked() bool {
	return p.Findings.Blocked()
}

// Unimplemented reports the tasks this build describes and cannot perform,
// which is what stops a stage before it starts.
func (p *Plan) Unimplemented() []Task {
	var out []Task
	for _, task := range p.Tasks {
		if task.Blocked != "" {
			out = append(out, task)
		}
	}
	return out
}

// Summary renders the counts §27 closes the plan with, one line per verb and
// kind, sorted so two runs over one cluster print the same report.
func (p *Plan) Summary() []string {
	type bucket struct {
		verb Verb
		kind string
	}
	counts := make(map[bucket]int)
	for _, action := range p.Actions() {
		kind := action.Object.GVK.Kind
		if kind == UpgradeKind {
			// Counting these by kind and verb says nothing: they all name one
			// subject, and the task count below is what a reader wants.
			continue
		}
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

	out := make([]string, 0, len(keys)+2)
	for _, key := range keys {
		out = append(out, fmt.Sprintf("%d %s will be %s.", counts[key], plural(key.kind, counts[key]), pastTense(key.verb)))
	}
	out = append(out, fmt.Sprintf("%d %s in total.", len(p.Tasks), plural("task", len(p.Tasks))))
	if blocked := len(p.Unimplemented()); blocked > 0 {
		out = append(out, fmt.Sprintf(
			"%d of them are described and not implemented, so %s cannot be run yet.",
			blocked, p.Stage))
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
	case VerbAwait:
		return "waited for"
	case VerbVerify:
		return "verified"
	default:
		return strings.ToLower(string(v))
	}
}
