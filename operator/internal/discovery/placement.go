// Which part of a worker a storage node should be pinned to.
//
// A storage node draws its SPDK cores, its huge-page memory, its data NIC, and
// its disks from one side of the interconnect, so on a two-socket machine the
// choice of side is the difference between a node that performs and one that
// crosses the interconnect for every I/O. The probe reports every reading per
// memory node so that this decision is possible at all.
//
// What is implemented is the naive answer: rank the memory nodes by how much
// unclaimed storage hangs off each and take the best. It is naive in ways worth
// naming, because each is a thing the next implementation should do and this one
// does not:
//
//   - It does not look at where the data NIC is. A node with four disks and no
//     fast NIC may be the wrong choice against one with three and a 100 GbE
//     port.
//   - It does not look at huge pages. A node with the disks and no reserved
//     memory cannot start SPDK at all, and this ranks it first.
//   - It does not consider using more than one memory node, which is what a
//     machine with disks evenly split across two sockets actually wants: two
//     storage nodes, one per socket.
//
// All three need either a policy the API does not carry or hardware nobody has
// described yet, which is why this is a seam and not a function.

package discovery

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// Placement is the choice of which of a worker's resources to use.
type Placement interface {
	Name() string

	// Choose picks from the devices already admitted by the device rules. It
	// returns the devices to use and a sentence saying what it chose and why,
	// which travels into the plan: an administrator reading a draft that names
	// four of a worker's eight disks is owed the reason.
	Choose(report nodeprobe.Report, admitted []nodeprobe.Device) (chosen []nodeprobe.Device, why string)
}

// NUMANodeResources is one memory node's share of a worker, as the ranking sees
// it.
type NUMANodeResources struct {
	// Node is the memory node, or inventory.NUMANodeUnknown for the devices
	// whose bus reported none.
	Node int

	// Devices are the admitted devices attached to it.
	Devices []nodeprobe.Device

	// DeviceBytes is their combined capacity.
	DeviceBytes uint64

	// OnlineCPUs and PhysicalCores are what the node has to run SPDK on.
	OnlineCPUs, PhysicalCores int

	// FreeHugePageBytes is the huge-page memory reserved on this node and not
	// yet taken.
	FreeHugePageBytes uint64

	// FastestNICMbps is the fastest link on this node, recorded but not ranked
	// on. See the file header.
	FastestNICMbps int
}

// MostAvailableNUMANode uses the memory node with the most unclaimed storage on
// it.
//
// Storage first and by count, not by capacity: a cluster's usable space is
// bounded by its erasure-coding stripe, which is a count of devices, so four
// small disks beat one large one for the same reason a stripe cannot be laid
// across a single device. Capacity breaks a tie in the count, cores break a tie
// in capacity, and the node id breaks the rest, so that two runs against one
// worker choose the same node.
type MostAvailableNUMANode struct{}

func (MostAvailableNUMANode) Name() string { return "most available NUMA node" }

func (p MostAvailableNUMANode) Choose(
	report nodeprobe.Report,
	admitted []nodeprobe.Device,
) ([]nodeprobe.Device, string) {
	nodes := NUMANodeBreakdown(report, admitted)
	if len(nodes) == 0 {
		return nil, "no memory node of it carries an unclaimed device"
	}
	if len(nodes) == 1 {
		only := nodes[0]
		return only.Devices, fmt.Sprintf(
			"every unclaimed device is on %s, so there was nothing to choose",
			describeNode(only.Node))
	}

	slices.SortFunc(nodes, func(a, b NUMANodeResources) int {
		return cmp.Or(
			cmp.Compare(len(b.Devices), len(a.Devices)),
			cmp.Compare(b.DeviceBytes, a.DeviceBytes),
			cmp.Compare(b.PhysicalCores, a.PhysicalCores),
			// A real memory node beats the bucket of devices that are on none.
			// Pinning to a node is the whole point of the placement, so "no
			// node in particular" is what to fall back to and never what to
			// prefer — and the bucket is numbered -1, so comparing ids alone
			// would rank it above node 0.
			//
			// The core count usually settles it first, because the unknown
			// bucket is credited with none. That is not a guarantee: a
			// worker whose CPU topology could not be read has no cores against
			// any node, which Collect tolerates and records rather than
			// failing on, and the comparison then falls through to here.
			cmp.Compare(rank(a.Node), rank(b.Node)),
			cmp.Compare(a.Node, b.Node),
		)
	})

	best, runnerUp := nodes[0], nodes[1]
	return best.Devices, fmt.Sprintf(
		"%s carries %d unclaimed devices (%s) against %d (%s) on %s, so it was chosen",
		describeNode(best.Node), len(best.Devices), humanBytes(best.DeviceBytes),
		len(runnerUp.Devices), humanBytes(runnerUp.DeviceBytes), describeNode(runnerUp.Node))
}

// NUMANodeBreakdown groups a worker's admitted devices by the memory node they
// hang off, and attaches what else that node has.
//
// It is exported because it is the evidence a better Placement needs, and
// because a caller reporting why a worker was placed where it was should not
// have to recompute it.
func NUMANodeBreakdown(report nodeprobe.Report, admitted []nodeprobe.Device) []NUMANodeResources {
	byNode := map[int]*NUMANodeResources{}
	order := []int{}

	node := func(id int) *NUMANodeResources {
		if existing, ok := byNode[id]; ok {
			return existing
		}
		byNode[id] = &NUMANodeResources{Node: id}
		order = append(order, id)
		return byNode[id]
	}

	for _, device := range admitted {
		entry := node(device.NUMANode)
		entry.Devices = append(entry.Devices, device)
		entry.DeviceBytes += device.SizeBytes
	}

	for _, cpus := range report.CPU.NUMANodes {
		if entry, ok := byNode[cpus.Node]; ok {
			entry.OnlineCPUs = len(cpus.OnlineCPUs)
			entry.PhysicalCores = cpus.PhysicalCores
		}
	}
	for _, pool := range report.HugePages {
		for _, share := range pool.NUMANodes {
			if entry, ok := byNode[share.Node]; ok {
				entry.FreeHugePageBytes += share.Free * pool.SizeBytes
			}
		}
	}
	for _, iface := range report.Interfaces {
		if entry, ok := byNode[iface.NUMANode]; ok && iface.SpeedMbps > entry.FastestNICMbps {
			entry.FastestNICMbps = iface.SpeedMbps
		}
	}

	slices.Sort(order)
	out := make([]NUMANodeResources, 0, len(order))
	for _, id := range order {
		out = append(out, *byNode[id])
	}
	return out
}

// rank orders a bucket ahead of or behind the others before ids are compared:
// every real memory node ranks the same, and the bucket that is not a node
// ranks after all of them.
func rank(node int) int {
	if node == inventory.NUMANodeUnknown {
		return 1
	}
	return 0
}

// describeNode names a memory node for a sentence, including the one that is not
// a node.
func describeNode(id int) string {
	if id == inventory.NUMANodeUnknown {
		return "no memory node in particular"
	}
	return fmt.Sprintf("NUMA node %d", id)
}

// AllDevices uses every admitted device, wherever it sits.
//
// It is the placement for a worker that should be used whole, and for a fleet
// whose machines have one memory node, where ranking them is arithmetic with
// one input.
type AllDevices struct{}

func (AllDevices) Name() string { return "all devices" }

func (AllDevices) Choose(_ nodeprobe.Report, admitted []nodeprobe.Device) ([]nodeprobe.Device, string) {
	return admitted, "every unclaimed device was used, without regard to its memory node"
}
