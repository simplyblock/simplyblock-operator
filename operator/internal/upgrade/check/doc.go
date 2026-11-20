// Package check holds the validations the three commands run. §18 is the set
// the migration performs and §19.10 is the eight the preflight does, and each
// of them is one upgrade.Check.
//
// A check reads the graph and reports findings. It never writes, it never
// decides what happens next, and what a finding does to the run follows from
// its severity rather than from the check that raised it. That split is what
// lets a check be added without changing the shape of a stage.
package check
