// Tests for the registry and the dependency ordering behind it. What is being
// asserted is that a catalog runs as it was written, that a rule cannot be
// silently replaced by one somebody added later, and that an ordering mistake
// is caught when the catalog is built rather than halfway through a migration.

package upgrade

import (
	"strings"
	"testing"
)

// namedRule is the smallest thing a registry can hold.
type namedRule struct {
	id       ID
	summary  string
	requires []ID
}

func (r namedRule) ID() ID              { return r.id }
func (r namedRule) Description() string { return r.summary }
func (r namedRule) Requires() []ID      { return r.requires }

func rule(id ID, requires ...ID) namedRule {
	return namedRule{id: id, summary: "a rule called " + string(id), requires: requires}
}

func TestRegistry_KeepsRegistrationOrder(t *testing.T) {
	reg := NewRegistry[namedRule]("rules").MustRegister(rule("c"), rule("a"), rule("b"))

	if got := ids(reg.All()); strings.Join(got, ",") != "c,a,b" {
		t.Fatalf("order = %v, want the order they were registered in: a catalog is "+
			"a sequence somebody wrote down", got)
	}
}

func TestRegistry_RefusesADuplicateIdentity(t *testing.T) {
	reg := NewRegistry[namedRule]("rules").MustRegister(rule("a"))

	err := reg.Register(rule("a"))
	if err == nil {
		t.Fatal("registering a second rule under one identity was accepted, which "+
			"silently replaces a rule somebody expects to run", err)
	}
	if !strings.Contains(err.Error(), "rules") {
		t.Fatalf("error = %q, want it to name the catalog the collision was in", err)
	}
}

func TestRegistry_RefusesARuleWithNoIdentity(t *testing.T) {
	if err := NewRegistry[namedRule]("rules").Register(rule("")); err == nil {
		t.Fatal("a rule with no identity was accepted, and nothing could name it in a report")
	}
}

func TestRegistry_WithoutLeavesTheReceiverAlone(t *testing.T) {
	reg := NewRegistry[namedRule]("rules").MustRegister(rule("a"), rule("b"), rule("c"))

	narrowed := reg.Without("b")
	if got := ids(narrowed.All()); strings.Join(got, ",") != "a,c" {
		t.Fatalf("narrowed = %v, want a,c", got)
	}
	if reg.Len() != 3 {
		t.Fatalf("the receiver holds %d rules, want the 3 it was built with", reg.Len())
	}
}

func TestRegistry_WithoutAnUnregisteredIdentityIsNotAnError(t *testing.T) {
	reg := NewRegistry[namedRule]("rules").MustRegister(rule("a"))

	if got := reg.Without("nothing-by-that-name").Len(); got != 1 {
		t.Fatalf("Len = %d, want 1: skipping a rule that does not exist is the "+
			"same outcome as skipping one that does", got)
	}
}

func TestTopoSort_PlacesARuleAfterWhatItRequires(t *testing.T) {
	sorted, err := topoSort([]namedRule{
		rule("delete-owner", "reparent"),
		rule("reparent", "validate"),
		rule("validate"),
	}, "step")
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}

	if got := ids(sorted); strings.Join(got, ",") != "validate,reparent,delete-owner" {
		t.Fatalf("order = %v, want validate,reparent,delete-owner", got)
	}
}

func TestTopoSort_IsStableInCatalogOrder(t *testing.T) {
	// Three rules that require nothing must come out in the order the catalog
	// declared them, not in whatever order a map iterated.
	for range 20 {
		sorted, err := topoSort([]namedRule{rule("c"), rule("a"), rule("b")}, "step")
		if err != nil {
			t.Fatalf("topoSort: %v", err)
		}
		if got := ids(sorted); strings.Join(got, ",") != "c,a,b" {
			t.Fatalf("order = %v, want c,a,b on every run", got)
		}
	}
}

func TestTopoSort_ReportsACycle(t *testing.T) {
	_, err := topoSort([]namedRule{
		rule("a", "b"),
		rule("b", "a"),
	}, "step")

	if err == nil {
		t.Fatal("a cycle was accepted, and the migration would have stopped partway through")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("error = %q, want it to name the rules in the cycle", err)
	}
}

func TestTopoSort_AnAbsentRequirementIsSatisfied(t *testing.T) {
	// A requirement naming a rule the command line skipped is absent rather
	// than missing, and the rule that named it still runs.
	sorted, err := topoSort([]namedRule{rule("b", "a-that-was-skipped")}, "step")
	if err != nil {
		t.Fatalf("topoSort: %v", err)
	}
	if got := ids(sorted); strings.Join(got, ",") != "b" {
		t.Fatalf("order = %v, want b", got)
	}
}

// ids renders a sorted slice for comparison.
func ids[T Rule](rules []T) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, string(rule.ID()))
	}
	return out
}
