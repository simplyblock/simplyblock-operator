// The node-size cases, §4 of the document: vCPU, memory, and the huge pages a
// host already has.
//
// simplyblock allocates its own huge pages, so what these cases vary is the
// baseline the new allocation is added to, not a prerequisite. A host with
// nothing set aside is the ordinary starting state and several of the cases are
// there to record that the generator treats it as a reason to propose nothing.

package main

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

func sizeCases() map[string]Case {
	one := func(report nodeprobe.Report) Case {
		return Case{Family: "size", Reports: []nodeprobe.Report{report},
			Nodes: kubeFleet([]nodeprobe.Report{report})}
	}
	fourDisks := disks(spread(3*tb, 4)...)

	cases := map[string]Case{
		"SIZE-01": one(host("worker-01", cpu(1, 4, 2), fourDisks)),
		"SIZE-02": one(host("worker-01", cpu(1, 2, 1), mem(16*gb, 14*gb, 16*gb), fourDisks)),
		"SIZE-03": one(host("worker-01", cpu(1, 98, 2), mem(1024*gb, 1000*gb, 1024*gb),
			pages(pagePool(gb, 256)), fourDisks)),
		"SIZE-04": one(host("worker-01", cpu(2, 49, 2), mem(1024*gb, 1000*gb, 512*gb, 512*gb),
			pages(pagePool(gb, 128, 128)), disks(spread(3*tb, 2, 2)...))),
		"SIZE-06": one(host("worker-01", cpu(1, 4, 2), mem(4*gb, 900*mb, 4*gb), fourDisks)),
		"SIZE-07": one(host("worker-01", cpu(1, 16, 2), mem(0, 0),
			unreadable("read /proc/meminfo: permission denied"), fourDisks)),
		"SIZE-09": one(host("worker-01", pages(pagePool(gb, 256, 256)),
			reserved(512*gb), disks(spread(3*tb, 2, 2)...))),
		"SIZE-11": one(host("worker-01", cpu(1, 16, 2), pages(pagePool(2*mb, 256)), fourDisks)),
		"SIZE-12": one(host("worker-01", cpu(1, 16, 2),
			pages(nodeprobe.HugePagePool{SizeBytes: gb, Total: 64, Free: 64}), fourDisks)),
		"SIZE-13": one(host("worker-01", pages(pagePool(gb, 0, 128)),
			disks(spread(3*tb, 2, 2)...))),
		"SIZE-14": one(host("worker-01", cpu(1, 16, 2), swap(8*gb, 0), fourDisks)),
		"SIZE-16": one(host("worker-01", cpu(1, 16, 2), mem(56*gb, 40*gb, 56*gb),
			reserved(200*gb), pages(pagePool(gb, 200)), fourDisks)),
		"SIZE-17": one(host("worker-01", cpu(1, 16, 2), mem(1024*gb, 1000*gb, 1024*gb),
			noPages(), fourDisks)),
		"SIZE-18": one(host("worker-01", cpu(1, 16, 2), pages(pagePool(gb, 256)),
			reserved(256*gb), fourDisks)),
		"SIZE-19": one(host("worker-01", cpu(1, 16, 2),
			pages(nodeprobe.HugePagePool{SizeBytes: gb, Total: 256, Free: 0,
				NUMANodes: []nodeprobe.NUMAHugePages{{Node: 0, Total: 256, Free: 0}}}),
			reserved(256*gb), fourDisks)),
		"SIZE-20": one(host("worker-01", cpu(1, 16, 2),
			pages(pagePool(2*mb, 1024), pagePool(gb, 32)), fourDisks)),
		"SIZE-21": one(host("worker-01", cpu(1, 16, 2), noPages(), fourDisks)),
	}

	// A fleet whose chosen memory nodes carry different core counts, which is
	// what the cluster's one vCPU count has to fit.
	uneven := []nodeprobe.Report{
		host("worker-01", cpu(1, 4, 2), disks(spread(3*tb, 4)...)),
		host("worker-02", cpu(1, 16, 2), disks(spread(3*tb, 4)...)),
		host("worker-03", cpu(1, 49, 2), disks(spread(3*tb, 4)...)),
	}
	cases["SIZE-05"] = Case{Family: "size", Reports: uneven, Nodes: kubeFleet(uneven)}

	// One worker of three has nothing set aside on the node it is placed on,
	// and the whole cluster's proposal follows from it.
	mixed := []nodeprobe.Report{
		host("worker-01", pages(pagePool(gb, 128, 128)), disks(spread(3*tb, 2, 2)...)),
		host("worker-02", noPages(), disks(spread(3*tb, 2, 2)...)),
		host("worker-03", pages(pagePool(gb, 128, 128)), disks(spread(3*tb, 2, 2)...)),
	}
	cases["SIZE-10"] = Case{Family: "size", Reports: mixed, Nodes: kubeFleet(mixed)}

	// What Kubernetes says about the machine, which is not what the machine
	// says about itself.
	held := host("worker-01", cpu(1, 16, 2), mem(256*gb, 240*gb, 256*gb), disks(spread(3*tb, 4)...))
	cases["SIZE-15"] = Case{
		Family: "size", Reports: []nodeprobe.Report{held},
		Nodes: []corev1.Node{kubeNode("worker-01",
			reachableAt(managementAddress("worker-01")),
			sized("32", "256Gi", "28", "180Gi"),
			schedulableHugePages("1Gi", "64Gi"))},
	}

	// The seam case: WorkerWasReadable is off by default and the controller
	// never turns it on.
	cases["SIZE-08"] = Case{
		Family: "size", Slug: "the-fully-readable-worker-rule",
		Note: "Driven in the discovery package with Planner{WorkerRules: []WorkerRule{WorkerHasDevices{}, " +
			"WorkerWasReadable{}}} over the SIZE-07 report, because the controller never substitutes a worker rule.",
	}

	slugs := map[string]string{
		"SIZE-01": "eight-logical-cpus",
		"SIZE-02": "two-physical-cores",
		"SIZE-03": "one-socket-196-vcpus",
		"SIZE-04": "two-sockets-196-vcpus",
		"SIZE-05": "a-fleet-of-uneven-core-counts",
		"SIZE-06": "four-gibibytes-of-memory",
		"SIZE-07": "an-unreadable-memory-reading",
		"SIZE-09": "pages-already-set-aside",
		"SIZE-10": "one-worker-with-nothing-set-aside",
		"SIZE-11": "a-reservation-under-a-gigabyte",
		"SIZE-12": "a-reservation-with-no-node-breakdown",
		"SIZE-13": "pages-on-the-node-not-chosen",
		"SIZE-14": "swap-in-use",
		"SIZE-15": "allocatable-far-under-capacity",
		"SIZE-16": "no-room-for-an-allocation-on-top",
		"SIZE-17": "room-and-nothing-set-aside",
		"SIZE-18": "a-reuse-instruction-with-nowhere-to-put-it",
		"SIZE-19": "every-page-promised-to-a-mapping",
		"SIZE-20": "two-page-sizes-at-once",
		"SIZE-21": "no-hugetlbfs-at-all",
	}
	gaps := map[string]string{
		"SIZE-03": "G-3", "SIZE-06": "G-4", "SIZE-09": "G-14", "SIZE-10": "G-15",
		"SIZE-14": "G-5", "SIZE-16": "G-16", "SIZE-17": "G-15 and G-16",
		"SIZE-18": "G-17", "SIZE-19": "G-18", "SIZE-21": "G-19",
	}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		entry.Gap = gaps[id]
		cases[id] = entry
	}
	return cases
}
