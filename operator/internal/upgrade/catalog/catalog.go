// The catalog the shipped tool runs: every discoverer, check, derivation, step,
// and transformation this release knows about, in the order they run in.
//
// It is one file on purpose. Adding a rule to the product is adding it here, in
// view of everything else that runs, rather than in an init function in the
// package that happens to define it. What that costs is one import per package
// of rules. What it buys is that the answer to what migrate actually does is a
// list somebody can read, and that a test can build a catalog holding one rule
// without the other forty deciding the outcome.
//
// The registries are empty at the moment. The framework, the commands, and the
// walk are in place, and operator/docs/designs/crd-redesign/design-api-upgrade.md
// is the inventory of what fills them. The discoverers come from §17, the
// checks from §18 and §19, the derivations from §19.2 and §19.3, the steps from
// §9.1 and §20, and the transformations from §16.

package catalog

import "github.com/simplyblock/simplyblock-operator/internal/upgrade"

// Default builds the catalog the commands run against.
func Default() *upgrade.Catalog {
	c := upgrade.NewCatalog()

	c.Discoverers.MustRegister(discoverers()...)
	c.Checks.MustRegister(checks()...)
	c.Derivations.MustRegister(derivations()...)
	c.Steps.MustRegister(steps()...)
	c.Transformations.MustRegister(transformations()...)

	return c
}

// discoverers build the graph the checks and steps read (§17).
func discoverers() []upgrade.Discoverer {
	return nil
}

// checks validate it (§18, §19.10).
func checks() []upgrade.Check {
	return nil
}

// derivations are the naming formulas the name checks walk (§19.2, §19.3).
func derivations() []upgrade.Derivation {
	return nil
}

// steps are the work of the upgrade and the migration (§9.1, §20, §21). Their
// order within a stage comes from what each one requires rather than from this
// slice, so a step added in the wrong place still runs in the right one.
func steps() []upgrade.Step {
	return nil
}

// transformations are the object mappings conversion cannot carry (§16).
func transformations() []upgrade.Transformation {
	return nil
}
