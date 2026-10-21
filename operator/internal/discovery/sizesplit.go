// The size split (design-superclusters.md §5.5): a role tier larger than one
// cluster may hold is drafted as several node sets, each the member cluster of
// a supercluster. Discovery writes the draft a reviewer approves, so it is where
// the storage-node limit is met rather than discovered at approval.
//
// Members of equal size fill at the same rate, take the same share of new
// volumes, and each cost the aggregate the same share of its capacity when one
// fails, so a tier is split into members that differ by at most one worker.
// Workers are dealt round-robin in failure-domain order, which spreads every
// domain over every member: a domain's loss then degrades every member a
// little rather than taking one down.

package discovery

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// maxNodeSetName is the longest node set name whose member name,
// <cluster>-<nodeset>-<suffix>, stays a DNS label under the longest cluster
// name the operator drafts (design-superclusters.md §5.3).
const maxNodeSetName = 23

// maxStorageNodesPerMember is the storage-node limit a member is held to.
const maxStorageNodesPerMember = 32

// splitMembers splits every tier the builder produced into the node sets of its
// members. A tier that fits one member and shares one region and one network is
// left as the builder drafted it.
func splitMembers(
	sets []simplyblockv1alpha2.NodeSet, workers []Worker, grouper Grouper, slots int32,
) []simplyblockv1alpha2.NodeSet {
	byName := make(map[string]Worker, len(workers))
	for _, worker := range workers {
		byName[worker.Name] = worker
	}
	if slots < 1 {
		slots = 1
	}
	capacity := max(1, maxStorageNodesPerMember/int(slots))

	out := make([]simplyblockv1alpha2.NodeSet, 0, len(sets))
	for _, tier := range sets {
		var tierWorkers []Worker
		for _, group := range tier.Groups {
			for _, name := range group.Workers {
				if worker, known := byName[name]; known {
					tierWorkers = append(tierWorkers, worker)
				}
			}
		}
		members, regions := dealTier(tierWorkers, capacity)
		if len(members) <= 1 {
			out = append(out, tier)
			continue
		}

		numbers := map[string]int{}
		for _, member := range members {
			numbers[member.region]++
			name := tier.Name
			if regions > 1 {
				name += "-" + member.region
			}
			set := simplyblockv1alpha2.NodeSet{Name: numberedName(name, numbers[member.region])}
			for _, group := range grouper.Group(member.workers) {
				set.Groups = append(set.Groups, nodeGroupOf(group))
			}
			out = append(out, set)
		}
	}
	return out
}

// member is one node set's workers, and the region they are in.
type member struct {
	region  string
	workers []Worker
}

// dealTier partitions a tier by what a member must share and deals each
// partition into members of at most capacity workers, and reports how many
// regions the tier spans.
func dealTier(workers []Worker, capacity int) ([]member, int) {
	partitions := map[string][]Worker{}
	regionOf := map[string]string{}
	regions := map[string]struct{}{}
	for _, worker := range workers {
		region := worker.Kube.Labels[corev1.LabelTopologyRegion]
		key := region + "\x00" + worker.Mgmt.Name + "\x00" + strings.Join(worker.Data, ",") + "\x00" + string(worker.Class)
		partitions[key] = append(partitions[key], worker)
		regionOf[key] = region
		regions[region] = struct{}{}
	}
	keys := make([]string, 0, len(partitions))
	for key := range partitions {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var members []member
	for _, key := range keys {
		for _, workers := range deal(partitions[key], capacity) {
			members = append(members, member{region: regionOf[key], workers: workers})
		}
	}
	return members, len(regions)
}

// deal splits one partition into the fewest members of at most capacity
// workers, sized within one worker of each other: workers sorted by failure
// domain, then by name, are dealt round-robin, so every domain is spread over
// every member.
func deal(workers []Worker, capacity int) [][]Worker {
	sorted := slices.Clone(workers)
	slices.SortFunc(sorted, func(a, b Worker) int {
		return cmp.Or(cmp.Compare(a.FailureDomain, b.FailureDomain), cmp.Compare(a.Name, b.Name))
	})
	count := (len(sorted) + capacity - 1) / capacity
	members := make([][]Worker, count)
	for i, worker := range sorted {
		members[i%count] = append(members[i%count], worker)
	}
	return members
}

// numberedName is base-n, with the base cut so the whole stays within
// maxNodeSetName and keeps its number.
func numberedName(base string, n int) string {
	suffix := fmt.Sprintf("-%d", n)
	if room := maxNodeSetName - len(suffix); len(base) > room {
		base = strings.TrimRight(base[:room], "-")
	}
	return base + suffix
}
