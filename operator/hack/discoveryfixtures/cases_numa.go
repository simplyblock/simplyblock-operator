// The NUMA topology cases, §3 of the document.
//
// The placement ranks a worker's memory nodes by unclaimed device count, then
// capacity, then physical cores, then a real node ahead of the bucket that is
// not one, then the node identifier. Each case here leaves exactly one of those
// comparisons deciding, so that a change to the ranking fails the case that
// names the rung it changed.

package main

import (
	"fmt"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// spread lays n disks of one size across the memory nodes given, one slot each
// in a bus per node, so that two workers built the same way name the same
// addresses.
func spread(size uint64, perNode ...int) []nodeprobe.Device {
	var out []nodeprobe.Device
	index := 0
	for node, count := range perNode {
		for i := 0; i < count; i++ {
			out = append(out, nvme(
				fmt.Sprintf("nvme%dn1", index),
				fmt.Sprintf("0000:%02x:00.%d", 0x5e+node*0x20, i),
				node, size))
			index++
		}
	}
	return out
}

func numaCases() map[string]Case {
	twoNode := func(reports ...nodeprobe.Report) Case {
		return Case{Family: "numa", Reports: reports, Nodes: kubeFleet(reports)}
	}

	oneNode := host("worker-01", cpu(1, 16, 2), disks(spread(3*tb, 4)...))

	unknownNode := func(devices ...nodeprobe.Device) []nodeprobe.Device {
		for i := range devices {
			devices[i].NUMANode = inventory.NUMANodeUnknown
		}
		return devices
	}

	cases := map[string]Case{
		"NUMA-01": twoNode(oneNode),
		"NUMA-02": twoNode(host("worker-01", disks(spread(3*tb, 2, 2)...))),
		"NUMA-03": twoNode(host("worker-01", disks(spread(3*tb, 1, 3)...))),
		"NUMA-04": twoNode(host("worker-01", disks(append(
			spread(8*tb, 2), spread(tb, 0, 3)...)...))),
		"NUMA-05": twoNode(host("worker-01", disks(append(
			spread(tb, 2), spread(2*tb, 0, 2)...)...))),
		"NUMA-06": twoNode(host("worker-01", cpuNodes(2, 8, 24), disks(spread(3*tb, 2, 2)...))),
		"NUMA-07": twoNode(host("worker-01", cpu(4, 16, 2), disks(spread(3*tb, 2, 2, 2, 2)...))),
		"NUMA-08": twoNode(host("worker-01", cpu(8, 8, 2),
			disks(spread(3*tb, 1, 1, 1, 1, 1, 3, 1, 1)...))),
		"NUMA-09": twoNode(host("worker-01", disks(unknownNode(spread(3*tb, 4)...)...))),
		"NUMA-10": twoNode(host("worker-01", disks(append(
			spread(3*tb, 2), unknownNode(
				nvme("nvme8n1", "0000:c0:00.0", 0, 3*tb),
				nvme("nvme9n1", "0000:c1:00.0", 0, 3*tb),
			)...)...))),
		"NUMA-12": {
			Family: "numa",
			Note: "Both workers hand over the same two addresses, and the grouper reads the " +
				"addresses rather than the topology, so they share a group.",
			Reports: []nodeprobe.Report{
				host("worker-01", cpu(1, 16, 2), disks(
					nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
					nvme("nvme1n1", "0000:5e:00.1", 0, 3*tb),
				)),
				host("worker-02", disks(
					nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
					nvme("nvme1n1", "0000:5e:00.1", 1, 3*tb),
				)),
			},
		},
		"NUMA-13": twoNode(host("worker-01", disks(spread(3*tb, 0, 10)...))),
		"NUMA-14": twoNode(host("worker-01", cpu(2, 49, 2),
			mem(1024*gb, 1000*gb, 512*gb, 512*gb),
			pages(pagePool(gb, 256, 256)),
			disks(spread(3*tb, 5, 5)...))),
		"NUMA-15": twoNode(host("worker-01", noNUMATopology(2, 8, 2), disks(spread(3*tb, 2, 2)...))),

		// The seam case: the controller never substitutes a placement, so there
		// is nothing to drive it from and the row carries its own reasoning.
		"NUMA-11": {
			Family: "numa", Slug: "all-devices-placement",
			Note: "Driven in the discovery package with Planner{Placement: AllDevices{}} over the " +
				"NUMA-02 fleet, because the controller always builds the default Planner.",
		},
	}

	// NUMA-12 states its workers directly rather than through the helper, so it
	// is the one case here that would otherwise carry no node objects.
	twelve := cases["NUMA-12"]
	twelve.Nodes = kubeFleet(twelve.Reports)
	cases["NUMA-12"] = twelve

	slugs := map[string]string{
		"NUMA-01": "one-memory-node",
		"NUMA-02": "two-nodes-evenly-split",
		"NUMA-03": "two-nodes-one-against-three",
		"NUMA-04": "count-beats-capacity",
		"NUMA-05": "capacity-breaks-the-count-tie",
		"NUMA-06": "cores-break-the-capacity-tie",
		"NUMA-07": "four-memory-nodes",
		"NUMA-08": "eight-memory-nodes",
		"NUMA-09": "every-device-on-no-node",
		"NUMA-10": "a-real-node-against-the-unknown-bucket",
		"NUMA-12": "one-node-and-two-node-workers-agree-on-addresses",
		"NUMA-13": "every-disk-on-the-second-node",
		"NUMA-14": "a-large-two-socket-worker",
		"NUMA-15": "no-memory-node-carries-cores",
	}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		cases[id] = entry
	}
	return cases
}
