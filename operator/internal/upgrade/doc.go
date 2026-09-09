// Package upgrade is the extension framework the API upgrade and the
// resource-model migration are written in.
//
// operator/docs/designs/crd-redesign/design-api-upgrade.md is the design. It
// describes three commands over one cluster: a read-only preflight that reports
// the checks and the plan, an upgrade that makes the cluster capable of running
// the new operator, and a migration that commits the resource-model changes
// conversion cannot carry. What all three have in common is that they are
// assembled out of small units, and that the set of units grows: every later
// API version brings new rules to check, new names to bound, and new objects to
// transform.
//
// So nothing here is a switch over a fixed list. Each kind of unit is an
// interface with one registry behind it, and adding a rule to the product means
// writing one value and putting it in one catalog:
//
//	Derivation      a formula that builds a Kubernetes identifier out of a name
//	                somebody chose, and the inputs this cluster would hand it
//	Check           a validation over the discovered graph, reporting findings
//	Discoverer      a walk that puts objects and their edges into the graph
//	Step            one unit of work: planned, skipped when done, applied, verified
//	Verification    a post-condition a step is not finished without
//	Transformation  an object-level mapping, for a renamed kind or a rewritten key
//
// Every one of them is a [Rule], which is to say it has a stable identity and a
// sentence describing it, so a report can name it and an operator can skip it.
//
// The framework holds no global state. A catalog is composed explicitly, which
// is what lets a test run one check against a fake client without the other
// forty deciding the outcome.
package upgrade
