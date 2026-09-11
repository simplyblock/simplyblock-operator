// The validation extension point. A check reads the discovered graph and
// reports findings, and it never writes and never decides what happens next.
// What a finding does to the run is the runner's decision, taken from its
// severity, so that adding a check cannot accidentally change the control flow
// of a stage.

package upgrade

import "context"

// Check is one validation. §18 lists the validations the migration performs and
// §19.10 the eight the preflight does, and each of them is one implementation
// of this interface.
//
// A check MUST NOT write to the cluster, including through a server-side dry
// run: the preflight promises that nothing changed, and a dry-run write is
// still a request the API server admits and mutating webhooks see.
type Check interface {
	Rule

	// Stages names the commands the check runs in. A check listed in more than
	// one runs again in each, which is deliberate: an object created between
	// the upgrade and the migration has never been checked.
	Stages() []Stage

	// Check examines the scope and reports what it found. Returning no findings
	// means the check passed. An error means the check could not be performed,
	// which is a different outcome from a finding and blocks the run on its
	// own: a check that could not read the cluster has proven nothing about it.
	Check(ctx context.Context, s *Scope) (Findings, error)
}

// CheckFunc adapts a function into a [Check], for a check whose whole
// implementation is one closure and that carries no configuration.
type CheckFunc struct {
	RuleID  ID
	Summary string
	RunIn   []Stage
	Fn      func(ctx context.Context, s *Scope) (Findings, error)
}

func (c CheckFunc) ID() ID {
	return c.RuleID
}
func (c CheckFunc) Description() string {
	return c.Summary
}
func (c CheckFunc) Stages() []Stage {
	return c.RunIn
}

func (c CheckFunc) Check(ctx context.Context, s *Scope) (Findings, error) {
	return c.Fn(ctx, s)
}

// RunsIn reports whether a check is registered for this stage.
func RunsIn(check Check, stage Stage) bool {
	for _, s := range check.Stages() {
		if s == stage {
			return true
		}
	}
	return false
}
