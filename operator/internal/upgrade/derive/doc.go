// Package derive holds every formula this product uses to build a Kubernetes
// identifier out of a name somebody chose, and the inputs a cluster hands each
// one.
//
// §19 of design-api-upgrade.md is the audit these rows come from, and its point
// is that none of it is checked anywhere: nothing validates a label or a name
// before writing it, and no field in the seventeen registered kinds carries a
// MaxLength marker. An overflow becomes a reconcile that retries forever, and
// the resource it was reconciling reports nothing about the name that caused
// it.
//
// Each row is one [Rule], declaring the formula and where the value is written.
// Declaring them as values rather than burying each in the code that writes the
// object is what lets one check cover all of them, and what lets a formula
// added by a later release be checked without a new check being written. The
// thresholds in the comments are the design's measured ones, so a test that
// disagrees with one has found either a formula change or an error in the
// audit.
package derive
