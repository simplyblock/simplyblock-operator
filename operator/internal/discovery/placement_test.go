// Which memory node the placement picks, and what it says about the choice.
//
// The ranking is by device count before capacity, and that ordering is the one
// substantive claim in this file: a cluster's usable space is bounded by its
// erasure-coding stripe, which is a count of devices, so four small disks are
// worth more than one large one and the test below pins that rather than
// leaving it to whichever comparison came first.

package discovery

import (
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/inventory"
)

func TestMostAvailableNUMANodeTakesTheNodeWithTheMostDisks(t *testing.T) {
	const tb = uint64(1) << 40
	worker := report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
		disk("nvme1n1", "0000:5f:00.0", 1, tb),
		disk("nvme2n1", "0000:af:00.0", 1, tb),
		disk("nvme3n1", "0000:b0:00.0", 1, tb),
	)

	chosen, why := MostAvailableNUMANode{}.Choose(worker, worker.Devices)

	got := addresses(chosen)
	want := []string{"0000:5f:00.0", "0000:af:00.0", "0000:b0:00.0"}
	if !slices.Equal(got, want) {
		t.Errorf("chose %v, want the three disks on node 1: %v", got, want)
	}
	for _, fragment := range []string{"NUMA node 1", "3 unclaimed devices", "NUMA node 0"} {
		if !strings.Contains(why, fragment) {
			t.Errorf("the reason %q does not mention %q; a draft naming three of four "+
				"disks owes the reviewer the comparison", why, fragment)
		}
	}
}

func TestMostAvailableNUMANodePrefersCountOverCapacity(t *testing.T) {
	// One 8 TB disk on node 0 against two 1 TB disks on node 1. A stripe is
	// laid across devices, so it cannot be laid across the single disk at all,
	// and the node with more of them wins despite holding a quarter the bytes.
	const tb = uint64(1) << 40
	worker := report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", 0, 8*tb),
		disk("nvme1n1", "0000:af:00.0", 1, tb),
		disk("nvme2n1", "0000:b0:00.0", 1, tb),
	)

	chosen, _ := MostAvailableNUMANode{}.Choose(worker, worker.Devices)

	if got := addresses(chosen); !slices.Equal(got, []string{"0000:af:00.0", "0000:b0:00.0"}) {
		t.Errorf("chose %v, want the two smaller disks: a stripe is a count of devices", got)
	}
}

func TestMostAvailableNUMANodeBreaksATieOnCapacityThenOnNode(t *testing.T) {
	const tb = uint64(1) << 40

	// Equal counts, unequal bytes: the larger pair wins.
	worker := report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
		disk("nvme1n1", "0000:5f:00.0", 0, tb),
		disk("nvme2n1", "0000:af:00.0", 1, 4*tb),
		disk("nvme3n1", "0000:b0:00.0", 1, 4*tb),
	)
	chosen, _ := MostAvailableNUMANode{}.Choose(worker, worker.Devices)
	if got := addresses(chosen); !slices.Equal(got, []string{"0000:af:00.0", "0000:b0:00.0"}) {
		t.Errorf("chose %v, want node 1's larger pair", got)
	}

	// Equal in every ranked term: the lower node id wins, so two runs against
	// one worker agree.
	even := report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
		disk("nvme1n1", "0000:af:00.0", 1, tb),
	)
	first, _ := MostAvailableNUMANode{}.Choose(even, even.Devices)
	second, _ := MostAvailableNUMANode{}.Choose(even, even.Devices)
	if !slices.Equal(addresses(first), []string{"0000:5e:00.0"}) {
		t.Errorf("chose %v on an even split, want node 0", addresses(first))
	}
	if !slices.Equal(addresses(first), addresses(second)) {
		t.Errorf("two runs chose %v and %v", addresses(first), addresses(second))
	}
}

func TestMostAvailableNUMANodeSaysWhenThereWasNothingToChoose(t *testing.T) {
	// A one-socket machine, or a machine whose disks all hang off one node. The
	// reason has to read differently from a comparison, because there was none.
	const tb = uint64(1) << 40
	worker := report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
		disk("nvme1n1", "0000:5f:00.0", 0, tb),
	)

	chosen, why := MostAvailableNUMANode{}.Choose(worker, worker.Devices)

	if len(chosen) != 2 {
		t.Errorf("chose %d of 2 disks on a single-node machine", len(chosen))
	}
	if !strings.Contains(why, "nothing to choose") {
		t.Errorf("the reason is %q, and no choice was made", why)
	}
}

func TestMostAvailableNUMANodeHandlesDevicesOnNoNode(t *testing.T) {
	// A disk behind a controller whose bus reports no memory node is still a
	// disk. It must not vanish, and it must not be mixed with a real node's.
	const tb = uint64(1) << 40
	worker := report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", inventory.NUMANodeUnknown, tb),
		disk("nvme1n1", "0000:af:00.0", 1, tb),
		disk("nvme2n1", "0000:b0:00.0", 1, tb),
	)

	chosen, why := MostAvailableNUMANode{}.Choose(worker, worker.Devices)

	if got := addresses(chosen); !slices.Equal(got, []string{"0000:af:00.0", "0000:b0:00.0"}) {
		t.Errorf("chose %v, want node 1's two", got)
	}
	if strings.Contains(why, "NUMA node -1") {
		t.Errorf("the reason %q names -1 as a node rather than saying there is none", why)
	}
}

func TestMostAvailableNUMANodeRefusesAWorkerWithNothingLeft(t *testing.T) {
	worker := report("worker-1")

	chosen, why := MostAvailableNUMANode{}.Choose(worker, nil)

	if len(chosen) != 0 {
		t.Errorf("chose %d devices from a worker with none", len(chosen))
	}
	if why == "" {
		t.Error("declined a worker without saying why")
	}
}

func TestNUMANodeBreakdownAttachesWhatElseTheNodeHas(t *testing.T) {
	// The breakdown is the evidence a better placement needs, so the readings
	// it carries are asserted even though the current ranking ignores them.
	const tb = uint64(1) << 40
	worker := report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
		disk("nvme2n1", "0000:af:00.0", 1, tb),
	)

	nodes := NUMANodeBreakdown(worker, worker.Devices)

	if len(nodes) != 2 {
		t.Fatalf("broke the worker into %d nodes, want 2", len(nodes))
	}
	if nodes[0].Node != 0 || nodes[1].Node != 1 {
		t.Errorf("the nodes are %d and %d, want them ascending", nodes[0].Node, nodes[1].Node)
	}
	for _, node := range nodes {
		if node.OnlineCPUs != 16 || node.PhysicalCores != 8 {
			t.Errorf("node %d has %d CPUs over %d cores, want 16 over 8",
				node.Node, node.OnlineCPUs, node.PhysicalCores)
		}
		if node.FreeHugePageBytes != 16<<30 {
			t.Errorf("node %d has %d free huge-page bytes, want %d",
				node.Node, node.FreeHugePageBytes, uint64(16)<<30)
		}
		if node.FastestNICMbps != 25000 {
			t.Errorf("node %d has a %d Mbps NIC, want 25000", node.Node, node.FastestNICMbps)
		}
	}
}

func TestAllDevicesTakesEverything(t *testing.T) {
	const tb = uint64(1) << 40
	worker := report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
		disk("nvme2n1", "0000:af:00.0", 1, tb),
	)

	chosen, why := AllDevices{}.Choose(worker, worker.Devices)

	if len(chosen) != 2 {
		t.Errorf("chose %d of 2", len(chosen))
	}
	if why == "" {
		t.Error("the placement said nothing about what it did")
	}
}
