// The per-object step, which is the shape most of this migration's work has.
//
// §22 states idempotency per object rather than per step: a StorageNode owned
// by its set is transferred, and one already owned by the cluster is already
// migrated and continues. §20 orders the reparenting the same way, object by
// object with a verification between. A step that answered those questions once
// for a whole kind would collapse three nodes into one all-or-nothing decision,
// which is exactly wrong for a run killed after the second of them.
//
// So an [ObjectStep] answers five questions about one object, and [PerObject]
// turns it into an ordinary [Step] by walking the graph. The phase graph, the
// registry, and the runner are unchanged, and the fan-out, the per-object
// skipping, and the per-object verification are implemented once here rather
// than in every step.
//
// Not every step has a subject. Deploying the conversion webhook, applying the
// CRDs, handing the Helm release over, and switching the storage version act on
// the installation rather than on a graph node, and they stay plain [Step]
// implementations. Giving them an invented subject would buy nothing.

package upgrade

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ObjectStep is one unit of work, expressed against one object at a time.
//
// The five methods split two questions that look like one. Describe asks
// whether this step is about this object at all, and Done asks whether the
// change it describes is already present. Keeping them apart is what makes a
// rerun's plan shrink as work completes, rather than listing the same actions
// until the migration finishes.
type ObjectStep interface {
	Rule

	// Stage is the command this step belongs to.
	Stage() Stage

	// Phase is the migrate state it runs in, ignored outside [StageMigrate].
	Phase() Phase

	// Requires names steps that must have completed first.
	Requires() []ID

	// Describe reports the change this step would make to this object, and nil
	// when the step has nothing to do with it. A nil is an explicit
	// declination rather than a silence, which is what lets the runner tell an
	// object no step is responsible for from one every step declined.
	//
	// It MUST NOT write, and it MUST be a pure function of the object and the
	// graph: it runs in the preflight, where nothing changes, and again in the
	// migration, where a different answer would apply something the user was
	// never shown.
	Describe(ctx context.Context, s *Scope, obj client.Object) (*Action, error)

	// Done reports whether the described change is already present on this
	// object. It is derived from the object rather than from a record, which is
	// what makes a rerun safe (§22.1).
	Done(ctx context.Context, s *Scope, obj client.Object) (bool, error)

	// Validate reports why this object is not ready for the change. It is the
	// step's own precondition, distinct from the graph-wide validations of the
	// Check registry, and it runs in the preflight too, so a user sees what is
	// not ready before anything is applied.
	Validate(ctx context.Context, s *Scope, obj client.Object) error

	// Apply performs the change on this object. The runner calls it only after
	// Describe returned an action, Done reported false, and Validate passed.
	Apply(ctx context.Context, s *Scope, obj client.Object) error

	// Verify confirms the change is present. It runs after Apply, and also on
	// an object Done reported true for, because a rerun that trusts Done and
	// skips the check cannot notice that the state it resumed from is not the
	// state it thinks.
	Verify(ctx context.Context, s *Scope, obj client.Object) error
}

// PerObject adapts an [ObjectStep] into a [Step] by walking the graph.
//
// The walk is the graph's own order, kinds sorted and objects in discovery
// order within a kind, so the plan a user reads and the sequence the migration
// performs are the same sequence twice rather than two orders that happen to
// agree.
func PerObject(step ObjectStep) Step { return objectStepRunner{step: step} }

// objectStepRunner is the adapter. It holds the contract that every per-object
// step would otherwise restate.
type objectStepRunner struct {
	step ObjectStep
}

func (r objectStepRunner) ID() ID              { return r.step.ID() }
func (r objectStepRunner) Description() string { return r.step.Description() }
func (r objectStepRunner) Stage() Stage        { return r.step.Stage() }
func (r objectStepRunner) Phase() Phase        { return r.step.Phase() }
func (r objectStepRunner) Requires() []ID      { return r.step.Requires() }

// Verifications is empty because verification is per object and happens inside
// Apply, on every subject including the ones that were already done. A
// step-level post-condition would have nothing left to assert that the subjects
// have not each asserted for themselves.
func (r objectStepRunner) Verifications() []Verification { return nil }

// Plan reports the outstanding work, which is the objects this step is about
// and has not already changed.
//
// An object the step describes and reports done contributes nothing, so a plan
// taken after a partial run shows what is left rather than what was originally
// intended.
func (r objectStepRunner) Plan(ctx context.Context, s *Scope) ([]Action, error) {
	var actions []Action
	for _, obj := range s.Graph.Objects() {
		action, err := r.step.Describe(ctx, s, obj)
		if err != nil {
			return nil, fmt.Errorf("describing %s: %w", s.Ref(obj), err)
		}
		if action == nil {
			continue
		}

		done, err := r.step.Done(ctx, s, obj)
		if err != nil {
			return nil, fmt.Errorf("asking whether %s was already changed: %w", s.Ref(obj), err)
		}
		if done {
			continue
		}
		actions = append(actions, *action)
	}
	return actions, nil
}

// Done reports whether every object this step is about has already been
// changed, which is what the runner asks before applying anything.
//
// A step with no subjects is done. That is not a special case: a migration
// whose StorageNodeSets are already retired has nothing for the reparenting to
// do, and reporting it as outstanding would leave the phase applying a step
// that walks an empty set.
func (r objectStepRunner) Done(ctx context.Context, s *Scope) (bool, error) {
	for _, obj := range s.Graph.Objects() {
		action, err := r.step.Describe(ctx, s, obj)
		if err != nil {
			return false, fmt.Errorf("describing %s: %w", s.Ref(obj), err)
		}
		if action == nil {
			continue
		}

		done, err := r.step.Done(ctx, s, obj)
		if err != nil {
			return false, fmt.Errorf("asking whether %s was already changed: %w", s.Ref(obj), err)
		}
		if !done {
			return false, nil
		}
	}
	return true, nil
}

// Apply walks the subjects, applying what is outstanding and verifying every
// one of them.
//
// The order within one object is the design's: validate, apply, verify. An
// object that was already done is verified and not reapplied, and an object
// whose precondition fails stops the step there rather than partway through the
// next one, because §25 requires that nothing downstream depend on an
// unverified change.
func (r objectStepRunner) Apply(ctx context.Context, s *Scope) error {
	for _, obj := range s.Graph.Objects() {
		ref := s.Ref(obj)

		action, err := r.step.Describe(ctx, s, obj)
		if err != nil {
			return fmt.Errorf("describing %s: %w", ref, err)
		}
		if action == nil {
			continue
		}

		done, err := r.step.Done(ctx, s, obj)
		if err != nil {
			return fmt.Errorf("asking whether %s was already changed: %w", ref, err)
		}

		if !done {
			if err := r.step.Validate(ctx, s, obj); err != nil {
				return fmt.Errorf("%s is not ready for this change: %w", ref, err)
			}
			s.Report.Item(action.String())
			if err := r.step.Apply(ctx, s, obj); err != nil {
				return fmt.Errorf("changing %s: %w", ref, err)
			}
		}

		if err := r.step.Verify(ctx, s, obj); err != nil {
			return fmt.Errorf("%s was changed and the change did not hold: %w", ref, err)
		}
	}
	return nil
}

// Subjects reports the objects a step is about, and is what the coverage of a
// migration is measured against: an object no step describes is one nothing has
// taken responsibility for.
func Subjects(ctx context.Context, s *Scope, step ObjectStep) ([]ObjectRef, error) {
	var out []ObjectRef
	for _, obj := range s.Graph.Objects() {
		action, err := step.Describe(ctx, s, obj)
		if err != nil {
			return nil, fmt.Errorf("describing %s: %w", s.Ref(obj), err)
		}
		if action != nil {
			out = append(out, s.Ref(obj))
		}
	}
	return out, nil
}
