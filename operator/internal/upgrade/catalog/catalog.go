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
// The registries are filled in as the work lands, and
// operator/docs/designs/crd-redesign/design-api-upgrade.md is the inventory of
// what goes in each. The discoverers come from §17, the checks from §18 and
// §19, the derivations from §19.2 and §19.3, the steps from §9.1 and §20, and
// the transformations from §16.

package catalog

import (
	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/check"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/derive"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/discover"
	"github.com/simplyblock/simplyblock-operator/internal/upgrade/steps"
)

// Default builds the catalog the commands run against.
func Default() *upgrade.Catalog {
	c := upgrade.NewCatalog()

	c.Discoverers.MustRegister(discoverers()...)
	c.Derivations.MustRegister(derivations()...)
	c.Steps.MustRegister(migrationSteps()...)
	c.Transformations.MustRegister(transformations()...)

	// The checks are registered last, because the ones that walk the naming
	// rules are handed the registry rather than a copy of it. That is what
	// makes --skip on a naming rule skip it inside the check as well, instead
	// of skipping only the line the rules command prints.
	c.Checks.MustRegister(checks(c)...)

	return c
}

// discoverers build the graph the checks and steps read (§17). They are grouped
// by where the objects come from rather than listed one by one, because the set
// of custom resources the migration reads moves with §7.2 and the set of core
// objects moves with whichever check needs one.
func discoverers() []upgrade.Discoverer {
	all := discover.SimplyblockKinds()
	all = append(all, discover.OwnedKinds()...)
	all = append(all, discover.CoreKinds()...)
	return append(all, discover.ClaimKinds()...)
}

// checks validate the graph (§18, §19.10).
func checks(c *upgrade.Catalog) []upgrade.Check {
	all := append(check.Names(c.Derivations),
		check.NamespaceCollapse(),
		check.AnnotationSpellings(),
	)
	all = append(all, check.Spine()...)
	return append(all, check.InFlight()...)
}

// derivations are the naming formulas the name checks walk (§19.2, §19.3). The
// labels are the tighter half, since a name copied into a label is held to 63
// bytes rather than 253.
func derivations() []upgrade.Derivation {
	return append(derive.Labels(), derive.Names()...)
}

// migrationSteps are the work of the upgrade and the migration (§9.1, §20,
// §21). Their order within a stage comes from what each one requires rather
// than from this slice, so a step added in the wrong place still runs in the
// right one.
func migrationSteps() []upgrade.Step {
	return steps.Ownership()
}

// transformations are the object mappings conversion cannot carry (§16).
func transformations() []upgrade.Transformation {
	return nil
}
