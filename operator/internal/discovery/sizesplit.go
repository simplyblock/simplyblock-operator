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
// little rather than taking one down. Workers carrying the placement-group
// label are assigned by it instead: one node set per value, which the split
// does not size (§5.7).

package discovery

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// maxNodeSetName is the longest node set name whose member name,
// <cluster>-<nodeset>-<suffix>, stays a DNS label under the longest cluster
// name the operator drafts (design-superclusters.md §5.3).
const maxNodeSetName = 23

// maxStorageNodesPerMember is the storage-node limit a member is held to.
const maxStorageNodesPerMember = 32

// splitMembers splits every tier the builder produced into the node sets of its
// members, and reports what the labels it read leave for a reviewer. A tier
// that fits one member and shares one region and one network is left as the
// builder drafted it.
func splitMembers(
	sets []simplyblockv1alpha2.NodeSet, workers []Worker, grouper Grouper, slots int32,
) ([]simplyblockv1alpha2.NodeSet, []Finding) {
	byName := make(map[string]Worker, len(workers))
	for _, worker := range workers {
		byName[worker.Name] = worker
	}
	if slots < 1 {
		slots = 1
	}
	capacity := max(1, maxStorageNodesPerMember/int(slots))

	out := make([]simplyblockv1alpha2.NodeSet, 0, len(sets))
	var findings []Finding
	for _, tier := range sets {
		var tierWorkers []Worker
		for _, group := range tier.Groups {
			for _, name := range group.Workers {
				if worker, known := byName[name]; known {
					tierWorkers = append(tierWorkers, worker)
				}
			}
		}
		members, regions, found := dealTier(tierWorkers, capacity)
		findings = append(findings, found...)
		if len(members) == 1 && members[0].name == "" {
			out = append(out, tier)
			continue
		}

		numbers := map[string]int{}
		for _, member := range members {
			name := member.name
			if name == "" {
				numbers[member.region]++
				base := tier.Name
				if regions > 1 {
					base += "-" + member.region
				}
				name = numberedName(base, numbers[member.region])
			}
			set := simplyblockv1alpha2.NodeSet{Name: name}
			for _, group := range grouper.Group(member.workers) {
				set.Groups = append(set.Groups, nodeGroupOf(group))
			}
			out = append(out, set)
		}
	}
	return out, findings
}

// member is one node set's workers, the region they are in, and the name a
// placement-group label gives it, empty for a member the split numbers.
type member struct {
	name    string
	region  string
	workers []Worker
}

// dealTier partitions a tier by what a member must share, and makes each
// partition's members: one per placement-group value its workers carry, and
// the rest dealt into members of at most capacity workers. It reports how many
// regions the tier spans, and what the placement-group labels leave for a
// reviewer.
func dealTier(workers []Worker, capacity int) ([]member, int, []Finding) {
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
	var findings []Finding
	valuePartitions := map[string]int{}
	for _, key := range keys {
		labeled, unlabeled, found := placementGroupsOf(partitions[key])
		findings = append(findings, found...)
		for _, value := range sortedValues(labeled) {
			valuePartitions[value]++
			members = append(members, member{name: value, region: regionOf[key], workers: labeled[value]})
		}
		if len(unlabeled) > 0 {
			for _, workers := range deal(unlabeled, capacity) {
				members = append(members, member{region: regionOf[key], workers: workers})
			}
		}
	}

	// A value whose workers are in two partitions cannot be one member. Each
	// partition's share stays a node set of its own, numbered so that the names
	// stay apart.
	numbered := map[string]int{}
	for i := range members {
		value := members[i].name
		if value == "" || valuePartitions[value] < 2 {
			continue
		}
		if numbered[value] == 0 {
			findings = append(findings, Finding{Reason: PlacementGroupInvalid, Message: fmt.Sprintf(
				"%s=%s is carried by workers in different regions or on different networks, which cannot "+
					"share a member, so its workers are drafted into %d node sets",
				PlacementGroupLabel, value, valuePartitions[value])})
		}
		numbered[value]++
		members[i].name = numberedName(value, numbered[value])
	}
	return members, len(regions), findings
}

// placementGroupsOf divides a partition's workers by the placement-group value
// they carry. A value that is not a node set name is reported, naming its
// workers, and they are dealt as unlabeled workers are. Where some workers
// carry a value and others none, the unlabeled ones are reported.
func placementGroupsOf(workers []Worker) (map[string][]Worker, []Worker, []Finding) {
	labeled := map[string][]Worker{}
	var unlabeled, missing []Worker
	invalid := map[string][]string{}
	for _, worker := range workers {
		value := worker.Kube.Labels[PlacementGroupLabel]
		switch {
		case value == "":
			unlabeled = append(unlabeled, worker)
			missing = append(missing, worker)
		case len(value) > maxNodeSetName || len(validation.IsDNS1123Label(value)) > 0:
			invalid[value] = append(invalid[value], worker.Name)
			unlabeled = append(unlabeled, worker)
		default:
			labeled[value] = append(labeled[value], worker)
		}
	}

	var findings []Finding
	for _, value := range sortedKeysOf(invalid) {
		findings = append(findings, Finding{Reason: PlacementGroupInvalid, Message: fmt.Sprintf(
			"%s carry %s=%q, which is not a node set name (a DNS label of at most %d characters), "+
				"so they are drafted as unlabeled workers are",
			strings.Join(invalid[value], ", "), PlacementGroupLabel, value, maxNodeSetName)})
	}
	if len(labeled) > 0 && len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for _, worker := range missing {
			names = append(names, worker.Name)
		}
		findings = append(findings, Finding{Reason: PlacementGroupLabelIncomplete, Message: fmt.Sprintf(
			"%s %s no %s label while other workers on the same network do, so %s split by size: "+
				"label %s to choose %s member",
			strings.Join(names, ", "), plural(len(names), "carries", "carry"), PlacementGroupLabel,
			plural(len(names), "it is", "they are"), plural(len(names), "it", "them"),
			plural(len(names), "its", "their"))})
	}
	return labeled, unlabeled, findings
}

// sortedValues is the placement-group values in order.
func sortedValues(labeled map[string][]Worker) []string {
	values := make([]string, 0, len(labeled))
	for value := range labeled {
		values = append(values, value)
	}
	slices.Sort(values)
	return values
}

// sortedKeysOf is the keys of a map in order.
func sortedKeysOf(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
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
