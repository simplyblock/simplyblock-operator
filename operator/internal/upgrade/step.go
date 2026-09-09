// The work extension point, and the post-condition that goes with it. A step is
// the unit both stages are assembled from, and its four methods are what make a
// stage restartable: the plan is what §27 prints, Done is §22's idempotency
// determination, Apply is the side effect, and the verifications are §21's rule
// that nothing is deleted before its replacement has been confirmed.

package upgrade

import "context"

// Step is one unit of work in a stage.
//
// The contract is what the runner relies on, and an implementation that breaks
// it breaks the guarantees the design makes to the user:
//
//   - Plan MUST NOT write. It runs in the preflight, where nothing changes.
//   - Done MUST derive its answer from the cluster rather than from a bookmark
//     (§22.1), so a run killed anywhere resumes correctly.
//   - Apply MAY be called only when Done reported false, and MUST be safe to
//     call again after a failure partway through.
//   - Verifications run after Apply, and a step whose verification fails stops
//     the stage. Nothing downstream may assume an unverified change.
type Step interface {
	Rule

	// Stage is the command this step belongs to.
	Stage() Stage

	// Phase is the migrate state the step runs in, and is ignored for a step
	// whose stage is not [StageMigrate]. Several steps may share a phase, and
	// they run in catalog order within it.
	Phase() Phase

	// Requires names steps that must have completed first. The runner refuses
	// a catalog whose requirements cannot be ordered, so an ordering mistake
	// is caught when the catalog is built rather than halfway through a
	// migration.
	Requires() []ID

	// Plan reports what the step would change without changing it.
	Plan(ctx context.Context, s *Scope) ([]Action, error)

	// Done reports whether the step's effect is already present.
	Done(ctx context.Context, s *Scope) (bool, error)

	// Apply performs the step.
	Apply(ctx context.Context, s *Scope) error

	// Verifications are the post-conditions the runner checks after Apply, in
	// order. A step with none is a step that cannot fail visibly, which is
	// only correct for one that changes nothing.
	Verifications() []Verification
}

// Verification is a post-condition. It is its own interface rather than a
// method on [Step] so that a verification can be registered, named in a report,
// and reused: several steps assert that a set of dependents survived their
// owner's deletion, and that assertion is one implementation.
type Verification interface {
	Rule

	// Verify reports why the post-condition does not hold. A nil error means it
	// does.
	Verify(ctx context.Context, s *Scope) error
}

// VerificationFunc adapts a function into a [Verification].
type VerificationFunc struct {
	RuleID  ID
	Summary string
	Fn      func(ctx context.Context, s *Scope) error
}

func (v VerificationFunc) ID() ID              { return v.RuleID }
func (v VerificationFunc) Description() string { return v.Summary }

func (v VerificationFunc) Verify(ctx context.Context, s *Scope) error {
	return v.Fn(ctx, s)
}
