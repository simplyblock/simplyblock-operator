// What a draft does when the fleet has more groups than one node set may hold.
//
// This became reachable when a block draft started naming devices by their
// persistent paths. Under kernel names a fleet of identical machines was one
// group however large it was, because every worker called its disks the same
// thing. Under persistent names each worker's disks are its own, so a fleet of
// real hardware produces one group per worker — and NodeSet.Groups is capped at
// 64, so the sixty-fifth worker made the draft something the API server refuses.
//
// A rejected draft is the worst of the outcomes: the run reports success, the
// document is written, and the create fails on a limit nothing in the run
// mentioned.

package discovery

import (
	"fmt"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// distinctWorkers is a fleet of n machines whose disks are their own, which is
// what real hardware reports and what produces one group per worker.
func distinctWorkers(n int) []Worker {
	workers := make([]Worker, 0, n)
	for i := range n {
		workers = append(workers, Worker{
			Name:  fmt.Sprintf("worker-%03d", i),
			Class: ClassBlock,
			Devices: []nodeprobe.Device{
				namedBlockDisk(fmt.Sprintf("/dev/disk/by-id/wwn-0x%016x", i)),
			},
		})
	}
	return workers
}

// Every group the fleet produced is in the draft, and no node set holds more
// than the API allows.
func TestAFleetLargerThanOneNodeSetIsStillDrafted(t *testing.T) {
	groups := (GroupByHardware{}).Group(distinctWorkers(150))
	if len(groups) != 150 {
		t.Fatalf("150 machines with their own disks made %d groups, want one each", len(groups))
	}

	sets := SingleNodeSet{}.Build(groups)

	total := 0
	for _, set := range sets {
		if len(set.Groups) > MaxGroupsPerNodeSet {
			t.Errorf("node set %s holds %d groups, and the API allows %d",
				set.Name, len(set.Groups), MaxGroupsPerNodeSet)
		}
		total += len(set.Groups)
	}
	if total != 150 {
		t.Errorf("the draft carries %d groups across %d node sets, want all 150",
			total, len(sets))
	}
}

// The node sets are named apart, or the document names two things the same and
// the API server refuses it for that instead.
func TestTheNodeSetsAFleetIsSplitIntoAreNamedApart(t *testing.T) {
	sets := SingleNodeSet{}.Build((GroupByHardware{}).Group(distinctWorkers(150)))
	if len(sets) < 2 {
		t.Fatalf("150 groups fitted into %d node set(s), and one holds %d",
			len(sets), MaxGroupsPerNodeSet)
	}

	seen := map[string]bool{}
	for _, set := range sets {
		if set.Name == "" {
			t.Error("a node set has no name")
		}
		if seen[set.Name] {
			t.Errorf("two node sets are called %s", set.Name)
		}
		seen[set.Name] = true
	}
}

// A fleet that fits is unchanged: one node set, with the name it always had.
// The split is what a fleet too large for one gets, not a new shape for every
// draft.
func TestAFleetThatFitsIsStillOneNodeSet(t *testing.T) {
	sets := SingleNodeSet{}.Build((GroupByHardware{}).Group(distinctWorkers(8)))

	if len(sets) != 1 {
		t.Fatalf("a fleet of 8 made %d node sets, want 1", len(sets))
	}
	if sets[0].Name != DefaultNodeSetName {
		t.Errorf("the node set is called %q, want %q", sets[0].Name, DefaultNodeSetName)
	}
}
