// The identity every extensible unit carries, and the registry that holds a set
// of them. Both are here rather than beside each interface because a registry
// of checks and a registry of steps differ only in their element type, and one
// generic implementation is what keeps their behavior identical.

package upgrade

import (
	"fmt"
	"slices"
	"strings"
)

// APIGroup is the group this migration upgrades. It is here rather than in each
// caller because [Scope.Occupied] and the discoverers both have to agree on
// which objects mark a namespace as this installation's.
const APIGroup = "storage.simplyblock.io"

// ID names one rule uniquely within its registry. It is written in kebab case
// and is stable across releases, because it appears in reports and is what an
// operator names when skipping a rule.
type ID string

// Rule is what every unit of the upgrade framework carries. The identity is for
// machines, reports, and the command line, while the description is the
// sentence a user reads when the rule blocks their upgrade.
type Rule interface {
	// ID is the rule's stable identity.
	ID() ID

	// Description says what the rule is for, in one sentence, in the present
	// tense, as in: bounds the node label a StorageCluster name is written into.
	Description() string
}

// Stage is which of the three commands a rule belongs to. A rule may belong to
// more than one: the name and identity checks run in all three, because an
// object created between two of them has never been checked.
type Stage string

const (
	// StagePreflight is the read-only command: the checks and the plan.
	StagePreflight Stage = "preflight"

	// StageUpgrade makes the cluster capable of running the new operator.
	StageUpgrade Stage = "upgrade"

	// StageMigrate performs the application-level resource migration.
	StageMigrate Stage = "migrate"
)

// Stages is every stage, in the order they are run in.
var Stages = []Stage{StagePreflight, StageUpgrade, StageMigrate}

// Describe is the sentence a progress view narrates while the stage runs. It is
// written as an activity rather than as a noun, because what a user watching a
// long run wants to know is what the process is doing to their cluster.
func (s Stage) Describe() string {
	switch s {
	case StagePreflight:
		return "Checking whether this cluster can be upgraded"
	case StageUpgrade:
		return "Preparing the cluster for the new operator"
	case StageMigrate:
		return "Migrating the resource model"
	default:
		return string(s)
	}
}

// Registry holds one set of rules, keyed by identity and kept in the order they
// were registered. Order matters for the units that have one, and a registry is
// deliberately not sorted by ID: a step catalog is a sequence somebody wrote
// down, and sorting it would silently reorder the upgrade.
//
// A Registry is not safe for concurrent registration. Catalogs are built once,
// at startup or at the top of a test, and read afterward.
type Registry[T Rule] struct {
	// name is what the registry is called in an error, so a duplicate
	// identity reports which catalog it collided in.
	name string

	order []ID
	rules map[ID]T
}

// NewRegistry builds an empty registry. The name appears in errors and is the
// plural of what it holds: checks, steps, or derivations.
func NewRegistry[T Rule](name string) *Registry[T] {
	return &Registry[T]{name: name, rules: make(map[ID]T)}
}

// Register adds rules in order, and reports the first identity that is empty or
// already taken. A duplicate is an error rather than an overwrite, because the
// rule that would have been silently replaced is one somebody wrote and expects
// to run.
func (r *Registry[T]) Register(rules ...T) error {
	for _, rule := range rules {
		id := rule.ID()
		if strings.TrimSpace(string(id)) == "" {
			return fmt.Errorf("a rule registered in %s has no identity", r.name)
		}
		if _, taken := r.rules[id]; taken {
			return fmt.Errorf("%s already holds a rule identified as %q", r.name, id)
		}
		r.rules[id] = rule
		r.order = append(r.order, id)
	}
	return nil
}

// MustRegister is [Registry.Register] for a catalog written as a literal, where
// a collision is a bug in the program rather than a runtime condition.
func (r *Registry[T]) MustRegister(rules ...T) *Registry[T] {
	if err := r.Register(rules...); err != nil {
		panic(err)
	}
	return r
}

// All returns every rule in registration order.
func (r *Registry[T]) All() []T {
	out := make([]T, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.rules[id])
	}
	return out
}

// Get returns the rule with this identity.
func (r *Registry[T]) Get(id ID) (T, bool) {
	rule, ok := r.rules[id]
	return rule, ok
}

// Len reports how many rules the registry holds.
func (r *Registry[T]) Len() int { return len(r.order) }

// Select returns the rules the predicate accepts, in registration order. It is
// how a runner narrows a catalog to one stage, and how the command line's skip
// list is applied.
func (r *Registry[T]) Select(keep func(T) bool) []T {
	out := make([]T, 0, len(r.order))
	for _, rule := range r.All() {
		if keep(rule) {
			out = append(out, rule)
		}
	}
	return out
}

// Without returns a registry holding everything but these identities, leaving
// the receiver untouched. An identity that is not registered is not an error:
// skipping a rule that does not exist is the same outcome as skipping one that
// does.
func (r *Registry[T]) Without(skip ...ID) *Registry[T] {
	out := NewRegistry[T](r.name)
	// MustRegister cannot collide here: the identities come from a registry
	// that already rejected duplicates.
	out.MustRegister(r.Select(func(rule T) bool {
		return !slices.Contains(skip, rule.ID())
	})...)
	return out
}
