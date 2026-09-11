// Package steps holds the work of the migration, one step per unit the design
// orders separately.
//
// The ownership steps of §20 are here first, because they are what the
// retirement of §16.1 turns on: a StorageNodeSet owns its StorageNode objects
// by controller reference, and it also owns the workload that runs them, so
// deleting a set today is what tears those objects down. Reparenting them onto
// the StorageCluster is what makes the deletion safe, and the ordering between
// the two is the whole point of the phase.
//
// Each step is asked about one subject at a time. What that buys, on a run
// killed after the second of three nodes, is that the third is the only one
// left: the step describes nothing for a node already owned by its cluster, so
// the plan shrinks and the rerun does what remains.
package steps
