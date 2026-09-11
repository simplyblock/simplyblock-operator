// The set of rules a run is assembled from. A catalog is composed explicitly
// rather than filled by package-level registration in init, so a test can run
// one check against a fake client without the other forty deciding the outcome,
// and so the order the steps run in is a sequence somebody wrote down rather
// than a consequence of import order.

package upgrade

// Catalog holds every rule a run may use, one registry per kind of rule.
// Extending the product means adding a value to one of these registries, and
// nothing about the runner changes.
type Catalog struct {
	// Discoverers build the graph (§17).
	Discoverers *Registry[Discoverer]

	// Checks validate it (§18, §19).
	Checks *Registry[Check]

	// Derivations are the naming formulas the name checks walk (§19.2, §19.3).
	Derivations *Registry[Derivation]

	// Steps are the work (§9.1, §20, §21).
	Steps *Registry[Step]

	// Transformations are the object mappings the migrate steps apply (§16).
	Transformations *Registry[Transformation]
}

// NewCatalog builds an empty catalog with every registry named.
func NewCatalog() *Catalog {
	return &Catalog{
		Discoverers:     NewRegistry[Discoverer]("discoverers"),
		Checks:          NewRegistry[Check]("checks"),
		Derivations:     NewRegistry[Derivation]("derivations"),
		Steps:           NewRegistry[Step]("steps"),
		Transformations: NewRegistry[Transformation]("transformations"),
	}
}

// ChecksFor returns the checks registered for a stage, in catalog order,
// leaving out the ones the run was told to skip.
func (c *Catalog) ChecksFor(stage Stage, opts Options) []Check {
	return c.Checks.Select(func(check Check) bool {
		return RunsIn(check, stage) && !opts.Skipped(check.ID())
	})
}

// StepsFor returns the steps of a stage in dependency order, or reports why the
// catalog cannot be ordered.
func (c *Catalog) StepsFor(stage Stage, opts Options) ([]Step, error) {
	steps := c.Steps.Select(func(step Step) bool {
		return step.Stage() == stage && !opts.Skipped(step.ID())
	})
	return orderSteps(steps)
}

// StepsInPhase returns the steps of one migrate phase, in dependency order.
func (c *Catalog) StepsInPhase(phase Phase, opts Options) ([]Step, error) {
	steps := c.Steps.Select(func(step Step) bool {
		return step.Stage() == StageMigrate && step.Phase() == phase && !opts.Skipped(step.ID())
	})
	return orderSteps(steps)
}

// TransformationsFrom returns the transformations that read a kind, in catalog
// order. It is how a step finds the mappings it applies without naming them.
func (c *Catalog) TransformationsFrom(source string, opts Options) []Transformation {
	return c.Transformations.Select(func(t Transformation) bool {
		return t.Source().Kind == source && !opts.Skipped(t.ID())
	})
}
