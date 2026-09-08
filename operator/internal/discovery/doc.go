// Package discovery turns what the probes reported into the document a reviewer
// approves.
//
// It is five decisions, and they are five seams rather than one function
// because each will be answered better later and none of them can be answered
// well now:
//
//  1. Which devices of a worker may go into a draft at all — [DeviceRule].
//  2. Which workers take part — [WorkerRule].
//  3. Which part of a worker to use, when a worker has more than one NUMA node
//     and only one of them is worth pinning a storage node to — [Placement].
//  4. Which workers share a configuration and so belong in one group —
//     [Grouper].
//  5. How the groups are organized into node sets — [NodeSetBuilder].
//
// [Planner] composes them and produces a ClusterDeploymentConfigSpec. Replacing
// any one of the five is replacing one field of a Planner.
//
// # What is implemented today, and what is not
//
// The implementations here are the simplest ones that are not wrong, which is a
// lower bar than the simplest ones imaginable in exactly one place. A single
// group holding every worker would be simpler than [GroupByHardware] and it
// would be false: a NodeGroup's device selection is shared by all its workers,
// so putting two workers with different PCI addressing in one group claims each
// has the other's disks. Grouping by what the workers actually have is
// therefore the floor rather than a refinement.
//
// The rest are deliberately naive and say so:
//
//   - [MostAvailableNUMANode] ranks a worker's memory nodes by how much
//     unclaimed storage hangs off each, and picks one. It does not consider
//     whether the data NIC is on the same node, or whether one node's disks are
//     faster, or whether two storage nodes should be placed on one worker.
//   - [SingleNodeSet] puts every group in one node set. A real fleet's node
//     sets are racks, and nothing in a probe report says which rack a worker is
//     in.
//   - [WorkerHasDevices] is the only worker rule. Nothing here reads a node's
//     labels, taints, capacity, or kubelet version to decide whether it should
//     take part.
//
// Each of those is a place where a better answer needs information the operator
// does not have yet, not a place where the answer was rushed. The seams are so
// that acquiring the information later does not mean rewriting the pipeline.
//
// # Nothing here fails a run over a device
//
// A rule refuses a device with a reason, and the reasons travel out in the
// [Plan] so that the run can say what it left behind and why. A discovery run
// that produced a draft without saying which disks it declined would leave an
// administrator comparing the draft against the hardware by hand, which is the
// job the run exists to do.
package discovery
