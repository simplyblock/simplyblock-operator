// The naming extension point. §19 of the design is a list of formulas this
// product uses to build a Kubernetes identifier out of a name somebody chose,
// each with a limit that binds it and an input length that overflows it.
// Declaring each formula as a value rather than burying it in the code that
// writes the object is what lets one check cover all of them, and what lets a
// formula added by a later release be checked without a new check being
// written.

package upgrade

import (
	"context"

	"github.com/simplyblock/atlas/kube"
)

// Derivation is one formula, together with every set of inputs the cluster
// under examination would hand it.
//
// The split between [Derivation.Formula] and [Derivation.Inputs] is what makes
// the two questions §19 asks answerable by one implementation: whether a value
// fits its limit is a property of the formula and one input, and whether two
// values collide is a property of the formula and all of them.
type Derivation interface {
	Rule

	// Formula is how the identifier is built. [kube.Formula] carries the limit
	// that binds it, which is not always the limit of the object the value is
	// written on: a name copied into a label is held to the label's 63 bytes.
	Formula() kube.Formula

	// Written says where the derived value ends up, in the form a report
	// prints: the Node label io.simplyblock.node-type, or a StorageClass name.
	Written() string

	// Inputs enumerates what this cluster would hand the formula. Each input
	// names the object the parts came from, so a violation can be reported
	// against something a user can edit.
	Inputs(ctx context.Context, s *Scope) ([]Input, error)
}

// Input is one set of parts a formula would be given, and where they came from.
type Input struct {
	// Source is the object whose name, or names, the parts are.
	Source ObjectRef

	// Parts are the formula's arguments, in order.
	Parts []string
}

// Derive runs the formula over the input.
func (d Input) Derive(formula kube.Formula) kube.Derived {
	return formula.Derive(d.Parts...)
}
