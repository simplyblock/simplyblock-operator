// Dependency ordering for the two kinds of rule that have one. A discoverer
// that walks the children of a StorageNodeSet names the discoverer that found
// the sets, and a step that deletes an old owner names the step that reparented
// its dependents, so neither depends on where somebody happened to put it in
// the catalog.
//
// The sort is stable in catalog order, so a catalog whose rules declare nothing
// runs exactly as it was written.

package upgrade

import (
	"fmt"
	"strings"
)

// ordered is what both sorts need from an element: an identity, and the
// identities it must follow.
type ordered interface {
	Rule
	Requires() []ID
}

// orderSteps sorts steps so that every step follows the ones it requires.
func orderSteps(steps []Step) ([]Step, error) {
	return topoSort(steps, "step")
}

// orderDiscoverers sorts discoverers the same way.
func orderDiscoverers(discoverers []Discoverer) ([]Discoverer, error) {
	return topoSort(discoverers, "discoverer")
}

// topoSort is Kahn's algorithm over the requirement edges, taking ready nodes
// in catalog order so the result is deterministic.
//
// A requirement naming a rule that is not in the set is satisfied rather than
// an error: a rule the command line skipped, or one that belongs to a stage
// this run is not performing, is not a missing dependency but an absent one.
// What a skipped rule's dependents do about that is their own decision, taken
// in Done.
func topoSort[T ordered](rules []T, noun string) ([]T, error) {
	present := make(map[ID]bool, len(rules))
	for _, rule := range rules {
		present[rule.ID()] = true
	}

	pending := make(map[ID]int, len(rules))
	unblocks := make(map[ID][]ID, len(rules))
	for _, rule := range rules {
		for _, required := range rule.Requires() {
			if !present[required] {
				continue
			}
			pending[rule.ID()]++
			unblocks[required] = append(unblocks[required], rule.ID())
		}
	}

	byID := make(map[ID]T, len(rules))
	for _, rule := range rules {
		byID[rule.ID()] = rule
	}

	// The queue is seeded in catalog order and refilled in catalog order, so
	// two runs over one catalog produce one sequence.
	var queue []ID
	for _, rule := range rules {
		if pending[rule.ID()] == 0 {
			queue = append(queue, rule.ID())
		}
	}

	out := make([]T, 0, len(rules))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, byID[id])

		var freed []ID
		for _, dependent := range unblocks[id] {
			pending[dependent]--
			if pending[dependent] == 0 {
				freed = append(freed, dependent)
			}
		}
		queue = append(queue, inCatalogOrder(rules, freed)...)
	}

	if len(out) != len(rules) {
		return nil, fmt.Errorf(
			"the %s catalog cannot be ordered: %s require each other in a cycle",
			noun, strings.Join(namesOf(rules, pending), ", "))
	}
	return out, nil
}

// inCatalogOrder reorders freed identities to match the catalog, so the queue
// never depends on map iteration order.
func inCatalogOrder[T ordered](rules []T, ids []ID) []ID {
	if len(ids) < 2 {
		return ids
	}
	want := make(map[ID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]ID, 0, len(ids))
	for _, rule := range rules {
		if want[rule.ID()] {
			out = append(out, rule.ID())
		}
	}
	return out
}

// namesOf lists the rules still blocked, which on a cycle is the cycle plus
// whatever waits behind it.
func namesOf[T ordered](rules []T, pending map[ID]int) []string {
	var out []string
	for _, rule := range rules {
		if pending[rule.ID()] > 0 {
			out = append(out, string(rule.ID()))
		}
	}
	return out
}
