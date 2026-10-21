// The size split (design-superclusters.md §5.5): every role tier is split into
// node sets of near-equal size within the storage-node limit, partitioned by
// what a member must share and dealt by failure domain.

package discovery

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// fleetOf is n workers with identical disks on eth0, which hardware grouping
// alone puts in one group, each changed by mutate.
func fleetOf(n int, mutate func(i int, w *Worker)) []Worker {
	workers := make([]Worker, 0, n)
	for i := range n {
		worker := Worker{
			Name:    fmt.Sprintf("worker-%03d", i),
			Class:   ClassBlock,
			Devices: []nodeprobe.Device{namedBlockDisk("/dev/disk/by-path/pci-0000:00:10.0-scsi-0:0:0:1")},
			Mgmt:    Management{Name: "eth0"},
			Kube:    KubeNode{Role: NodeRoleWorker},
		}
		if mutate != nil {
			mutate(i, &worker)
		}
		workers = append(workers, worker)
	}
	return workers
}

// distinctWorkers is a fleet of n machines whose disks are their own, which is
// what real hardware reports and what produces one group per worker.
func distinctWorkers(n int) []Worker {
	return fleetOf(n, func(i int, w *Worker) {
		w.Devices = []nodeprobe.Device{namedBlockDisk(fmt.Sprintf("/dev/disk/by-id/wwn-0x%016x", i))}
	})
}

// split runs the role split and then the size split over the workers.
func split(workers []Worker, slots int32) []simplyblockv1alpha2.NodeSet {
	grouper := GroupByHardware{}
	return splitMembers(SplitByRole{}.Build(grouper.Group(workers)), workers, grouper, slots)
}

// sizes is how many workers each node set lists, by name.
func sizes(sets []simplyblockv1alpha2.NodeSet) map[string]int {
	out := map[string]int{}
	for _, set := range sets {
		for _, group := range set.Groups {
			out[set.Name] += len(group.Workers)
		}
	}
	return out
}

// sortedSizes is the node sets' sizes, largest first.
func sortedSizes(sets []simplyblockv1alpha2.NodeSet) []int {
	bySet := sizes(sets)
	out := make([]int, 0, len(bySet))
	for _, n := range bySet {
		out = append(out, n)
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

func TestTwentyWorkersAreOneNodeSetKeepingTheTiersName(t *testing.T) {
	sets := split(fleetOf(20, nil), 1)
	if len(sets) != 1 || sets[0].Name != DefaultNodeSetName || sizes(sets)[DefaultNodeSetName] != 20 {
		t.Errorf("sets = %v, want one %s of 20", sizes(sets), DefaultNodeSetName)
	}
}

func TestTheSplitSizesMembersEvenly(t *testing.T) {
	for _, tc := range []struct {
		workers int
		slots   int32
		want    []int
	}{
		{32, 1, []int{32}},
		{33, 1, []int{17, 16}},
		{40, 1, []int{20, 20}},
		{40, 2, []int{14, 13, 13}},
		{65, 1, []int{22, 22, 21}},
	} {
		got := sortedSizes(split(fleetOf(tc.workers, nil), tc.slots))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%d workers at %d slot(s): sizes %v, want %v", tc.workers, tc.slots, got, tc.want)
		}
	}
}

// Every failure domain is spread over every member, so a domain's count in
// two members differs by at most one.
func TestFailureDomainsAreSpreadOverEveryMember(t *testing.T) {
	domains := []string{"zone-a", "zone-b", "zone-c"}
	sets := split(fleetOf(70, func(i int, w *Worker) { w.FailureDomain = domains[i%3] }), 1)
	if len(sets) != 3 {
		t.Fatalf("%d node sets, want 3", len(sets))
	}
	for _, domain := range domains {
		counts := make([]int, 0, len(sets))
		for _, set := range sets {
			n := 0
			for _, group := range set.Groups {
				if group.FailureDomain == domain {
					n += len(group.Workers)
				}
			}
			counts = append(counts, n)
		}
		if slices.Max(counts)-slices.Min(counts) > 1 {
			t.Errorf("%s per member = %v, want counts differing by at most one", domain, counts)
		}
	}
}

// Thirty workers in zone a and ten in zone b are two members of fifteen and
// five each.
func TestAnUnevenZoneIsDealtEvenly(t *testing.T) {
	sets := split(fleetOf(40, func(i int, w *Worker) {
		w.FailureDomain = "a"
		if i >= 30 {
			w.FailureDomain = "b"
		}
	}), 1)
	for _, set := range sets {
		perDomain := map[string]int{}
		for _, group := range set.Groups {
			perDomain[group.FailureDomain] += len(group.Workers)
		}
		if perDomain["a"] != 15 || perDomain["b"] != 5 {
			t.Errorf("node set %s holds %v, want 15 in a and 5 in b", set.Name, perDomain)
		}
	}
}

// workerSets maps every worker to the node set it is drafted into.
func workerSets(sets []simplyblockv1alpha2.NodeSet) map[string]string {
	out := map[string]string{}
	for _, set := range sets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				out[worker] = set.Name
			}
		}
	}
	return out
}

func TestWorkersOnDifferentManagementInterfacesAreNeverOneNodeSet(t *testing.T) {
	workers := fleetOf(12, func(i int, w *Worker) {
		if i%2 == 1 {
			w.Mgmt.Name = "eth1"
		}
	})
	placed := workerSets(split(workers, 1))
	for _, a := range workers {
		for _, b := range workers {
			if a.Mgmt.Name != b.Mgmt.Name && placed[a.Name] == placed[b.Name] {
				t.Fatalf("%s on %s and %s on %s share node set %s", a.Name, a.Mgmt.Name, b.Name, b.Mgmt.Name, placed[a.Name])
			}
		}
	}
}

// A partition too small for any scheme is drafted as it is, and validation
// reports the minimum.
func TestASmallPartitionIsNotPadded(t *testing.T) {
	workers := fleetOf(32, func(i int, w *Worker) {
		if i >= 30 {
			w.Mgmt.Name = "eth1"
		}
	})
	if got := sortedSizes(split(workers, 1)); !slices.Equal(got, []int{30, 2}) {
		t.Errorf("sizes = %v, want 30 and 2", got)
	}
}

func TestGroupNamesAreUniqueWithinEveryNodeSet(t *testing.T) {
	for _, set := range split(distinctWorkers(80), 1) {
		seen := map[string]bool{}
		for _, group := range set.Groups {
			if seen[group.Name] {
				t.Errorf("node set %s names group %s twice", set.Name, group.Name)
			}
			seen[group.Name] = true
		}
	}
}

// No node set lists more groups than workers, so none goes past 32 however
// distinct the disks are.
func TestTwoHundredDistinctWorkersMakeNoNodeSetAboveThirtyTwoGroups(t *testing.T) {
	for _, set := range split(distinctWorkers(200), 1) {
		if len(set.Groups) > 32 {
			t.Errorf("node set %s holds %d groups", set.Name, len(set.Groups))
		}
	}
}

func TestTheSplitIsTheSameForTheSameFleet(t *testing.T) {
	workers := fleetOf(70, func(i int, w *Worker) { w.FailureDomain = fmt.Sprintf("zone-%d", i%4) })
	shuffled := slices.Clone(workers)
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	first, second := workerSets(split(workers, 1)), workerSets(split(shuffled, 1))
	for name, set := range first {
		if second[name] != set {
			t.Errorf("%s is in %s on one run and %s on the other", name, set, second[name])
		}
	}
}

// Each tier splits on its own: an infrastructure tier of ten stays one node
// set, and forty workers become two.
func TestTiersSplitIndependently(t *testing.T) {
	sets := split(fleetOf(50, func(i int, w *Worker) {
		if i < 10 {
			w.Kube.Role = NodeRoleInfra
		}
	}), 1)
	got := sizes(sets)
	infra := NodeRoleInfra.NodeSetName()
	want := map[string]int{infra: 10, DefaultNodeSetName + "-1": 20, DefaultNodeSetName + "-2": 20}
	if len(got) != len(want) {
		t.Fatalf("sets = %v, want %v", got, want)
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("sets = %v, want %v", got, want)
		}
	}
	if sets[0].Name != infra {
		t.Errorf("first node set is %s, want the infrastructure tier first", sets[0].Name)
	}
}

// A member never spans two regions, and node sets of a tier spanning regions
// carry the region before their number.
func TestRegionsAreNeverDraftedIntoOneNodeSet(t *testing.T) {
	sets := split(fleetOf(8, func(i int, w *Worker) {
		region := "eu-west-1"
		if i%2 == 1 {
			region = "us-east-1"
		}
		w.Kube.Labels = map[string]string{corev1.LabelTopologyRegion: region}
	}), 1)
	got := sizes(sets)
	if got[DefaultNodeSetName+"-eu-west-1-1"] != 4 || got[DefaultNodeSetName+"-us-east-1-1"] != 4 || len(got) != 2 {
		t.Errorf("sets = %v, want four workers in each region's node set", got)
	}
}

// A generated name is cut so the member name it makes stays a DNS label,
// keeping its number.
func TestAGeneratedNameIsCutKeepingItsNumber(t *testing.T) {
	grouper := GroupByHardware{}
	workers := fleetOf(40, nil)
	sets := splitMembers(SplitByRole{SetName: "a-node-set-name-far-too-long"}.Build(grouper.Group(workers)), workers, grouper, 1)
	for i, set := range sets {
		suffix := fmt.Sprintf("-%d", i+1)
		if len(set.Name) > maxNodeSetName || !strings.HasSuffix(set.Name, suffix) {
			t.Errorf("node set %d is named %q, want at most %d characters ending in %s", i, set.Name, maxNodeSetName, suffix)
		}
	}
}
