// The work extension point. A step answers five questions about one subject,
// and the runner asks them of every subject a run has.
//
// The five are not one question in disguise. §22 states idempotency per object
// rather than per step, and §20 orders the reparenting the same way: a
// StorageNode owned by its set is transferred, one already owned by the cluster
// is already migrated and continues, and each move is verified before the next.
// A step that answered once for a whole kind would collapse three nodes into
// one all-or-nothing decision, which is the wrong answer for a run killed after
// the second of them.

package upgrade

import (
	"context"
	"fmt"
)

// Step is one unit of work.
//
// The contract is what the runner relies on, and an implementation that breaks
// it breaks a guarantee the design makes to the user:
//
//   - Describe and Validate MUST NOT write. Both run in the preflight, where
//     nothing changes.
//   - Describe MUST be a pure function of its subject and the graph. It runs in
//     the preflight and again in the migration, and a different answer applies
//     something the user was never shown.
//   - Describe and Done MUST derive their answers from the subject rather than
//     from a record (§22.1), so a run killed anywhere resumes correctly.
//   - Apply is called only where Describe returned an action and Validate
//     passed.
//   - Verify runs after Apply.
type Step interface {
	Rule

	// Stage is the command this step belongs to.
	Stage() Stage

	// Phase is the migrate state it runs in, ignored outside [StageMigrate].
	// Several steps may share a phase, and they run in catalog order within it.
	Phase() Phase

	// Requires names steps that must have completed first. The runner refuses
	// a catalog whose requirements cannot be ordered, so an ordering mistake is
	// caught when the catalog is built rather than halfway through a migration.
	Requires() []ID

	// Describe reports the change this step would make to this subject, and nil
	// when there is no operation to perform: the step is not about this subject
	// at all, or the subject is already in the state the step would put it in.
	//
	// A StorageNode already owned by its StorageCluster is the second case, and
	// it describes nothing. So the plan is the outstanding work by
	// construction, and a rerun's plan shrinks as the migration completes
	// rather than listing what was originally intended.
	//
	// This is also what makes a rerun self-healing. Describe inspects the
	// subject to decide, so a subject whose state is not what a previous run
	// left behind describes an action again and is put right, where asking a
	// separate question first and trusting the answer would report a change
	// that does not hold.
	Describe(ctx context.Context, s *Scope, subject Subject) (*Action, error)

	// Done reports whether the subject is in the state this step exists to put
	// it in.
	//
	// It is what tells the two halves of a nil Describe apart. A subject no
	// step describes is either finished or one nothing has taken
	// responsibility for, and those are very different answers to whether the
	// migration is complete, so the coverage question needs both. The runner
	// asks it only to report.
	Done(ctx context.Context, s *Scope, subject Subject) (bool, error)

	// Validate reports why this subject is not ready for the change. It is the
	// step's own precondition, distinct from the graph-wide validations of the
	// Check registry, and it runs in the preflight so a user sees what is not
	// ready before anything is applied.
	Validate(ctx context.Context, s *Scope, subject Subject) error

	// Apply performs the change.
	Apply(ctx context.Context, s *Scope, subject Subject) error

	// Verify confirms the change is present, and runs after Apply.
	//
	// It does not run on a subject Describe declined. There is nothing to
	// confirm there, and the inspection that would confirm it is the one
	// Describe already made.
	Verify(ctx context.Context, s *Scope, subject Subject) error
}

// Coverage is what a step has to say about the subjects of a run, which is what
// the completeness of a migration is measured against.
type Coverage struct {
	// Outstanding are the subjects the step described work for.
	Outstanding []Subject

	// Finished are the subjects it declined and reports as already in the
	// state it exists to produce.
	Finished []Subject

	// Untouched are the subjects it declined and does not claim. A subject
	// every step leaves here is one nothing has taken responsibility for.
	Untouched []Subject
}

// Covered reports what this step has to say about every subject of a run.
func Covered(ctx context.Context, s *Scope, step Step) (Coverage, error) {
	var out Coverage
	for _, subject := range s.Subjects() {
		action, err := step.Describe(ctx, s, subject)
		if err != nil {
			return out, fmt.Errorf("step %q could not describe %s: %w", step.ID(), subject, err)
		}
		if action != nil {
			out.Outstanding = append(out.Outstanding, subject)
			continue
		}

		done, err := step.Done(ctx, s, subject)
		if err != nil {
			return out, fmt.Errorf("step %q could not report whether %s was finished: %w",
				step.ID(), subject, err)
		}
		if done {
			out.Finished = append(out.Finished, subject)
			continue
		}
		out.Untouched = append(out.Untouched, subject)
	}
	return out, nil
}
