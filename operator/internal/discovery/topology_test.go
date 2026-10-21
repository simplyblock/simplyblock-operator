// The Kubernetes topology a draft reads off every worker's Node object
// (design-superclusters.md §5.7): the failure-domain label, by default
// topology.kubernetes.io/zone, and what the draft does when only some workers
// carry it.

package discovery

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// withLabels is a worker's Node object carrying the labels given.
func withLabels(name string, labels map[string]string) corev1.Node {
	node := kubeWorker(name)
	node.Labels = labels
	return node
}

// zonedPlan plans a uniform fleet of the workers given, whose Node objects
// carry the labels given, under the failure-domain label the run names.
func zonedPlan(label string, labels map[string]map[string]string) Plan {
	nodes := make([]corev1.Node, 0, len(labels))
	for name, set := range labels {
		nodes = append(nodes, withLabels(name, set))
	}
	run := &simplyblockv1alpha2.DiscoverSpec{FailureDomainLabel: label}
	return Planner{KubeNodes: KubeNodesOf(nodes)}.Plan(uniformFleet(len(labels)), run)
}

// domainsByWorker maps every drafted worker to its group's failure domain.
func domainsByWorker(plan Plan) map[string]string {
	out := map[string]string{}
	for _, set := range plan.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				out[worker] = group.FailureDomain
			}
		}
	}
	return out
}

// findingUnder is the plan's first finding under the reason, or none.
func findingUnder(plan Plan, reason string) *Finding {
	for i := range plan.Findings {
		if plan.Findings[i].Reason == reason {
			return &plan.Findings[i]
		}
	}
	return nil
}

func TestAWorkersZoneIsItsGroupsFailureDomain(t *testing.T) {
	plan := zonedPlan("", map[string]map[string]string{
		"worker-1": {corev1.LabelTopologyZone: "eu-west-1a"},
		"worker-2": {corev1.LabelTopologyZone: "eu-west-1b"},
	})

	got := domainsByWorker(plan)
	if got["worker-1"] != "eu-west-1a" || got["worker-2"] != "eu-west-1b" {
		t.Errorf("failure domains = %v, want each worker's zone verbatim", got)
	}
	if !plan.FailureDomains {
		t.Error("every worker carries a zone and the plan does not enable failure domains")
	}
}

func TestTheRunsFailureDomainLabelReplacesTheZone(t *testing.T) {
	plan := zonedPlan("example.com/rack", map[string]map[string]string{
		"worker-1": {corev1.LabelTopologyZone: "zone-a", "example.com/rack": "rack-1"},
		"worker-2": {corev1.LabelTopologyZone: "zone-a", "example.com/rack": "rack-2"},
	})

	if got := domainsByWorker(plan); got["worker-1"] != "rack-1" || got["worker-2"] != "rack-2" {
		t.Errorf("failure domains = %v, want each worker's rack", got)
	}
}

// A value the failureDomain field refuses is reported, naming the worker, and
// is not rewritten into one it accepts.
func TestAFailureDomainThatDoesNotFitIsReportedAndNotRewritten(t *testing.T) {
	plan := zonedPlan("example.com/rack", map[string]map[string]string{
		"worker-1": {"example.com/rack": "rack 1/a"},
		"worker-2": {"example.com/rack": "rack-2"},
	})

	if got := domainsByWorker(plan); got["worker-1"] != "" {
		t.Errorf("worker-1's failure domain = %q, want none", got["worker-1"])
	}
	found := findingUnder(plan, FailureDomainInvalid)
	if found == nil || !strings.Contains(found.Message, "worker-1") || !strings.Contains(found.Message, "rack 1/a") {
		t.Errorf("%s = %+v, want it to name worker-1 and its value", FailureDomainInvalid, found)
	}
}

// Every worker labeled enables failure domains on the template; no worker
// labeled leaves them unset and reports nothing.
func TestTheTemplateEnablesFailureDomainsOnlyWhenEveryWorkerHasOne(t *testing.T) {
	all := zonedPlan("", map[string]map[string]string{
		"worker-1": {corev1.LabelTopologyZone: "a"}, "worker-2": {corev1.LabelTopologyZone: "b"},
	})
	none := zonedPlan("", map[string]map[string]string{"worker-1": nil, "worker-2": nil})

	for _, tc := range []struct {
		name string
		plan Plan
		want bool
	}{{"all", all, true}, {"none", none, false}} {
		template, err := ClusterTemplateFor("c", tc.plan, TemplateOptions{})
		if err != nil {
			t.Fatalf("%s: ClusterTemplateFor: %v", tc.name, err)
		}
		got := template.Template.EnableFailureDomains != nil && *template.Template.EnableFailureDomains
		if got != tc.want {
			t.Errorf("%s labeled: enableFailureDomains = %v, want %v", tc.name, got, tc.want)
		}
	}
	if len(none.Findings) != 0 {
		t.Errorf("an unlabeled fleet reported %+v, want nothing", none.Findings)
	}
}

// Some workers labeled is drafted, with the unlabeled ones named.
func TestSomeWorkersWithoutAFailureDomainAreReported(t *testing.T) {
	plan := zonedPlan("", map[string]map[string]string{
		"worker-1": {corev1.LabelTopologyZone: "a"}, "worker-2": nil,
	})

	if plan.FailureDomains {
		t.Error("a fleet with an unlabeled worker enables failure domains")
	}
	found := findingUnder(plan, TopologyIncomplete)
	if found == nil || !strings.Contains(found.Message, "worker-2") || strings.Contains(found.Message, "worker-1") {
		t.Errorf("%s = %+v, want it to name worker-2 alone", TopologyIncomplete, found)
	}
}

// fastGroup is the placement-group value the tests label most workers with.
const fastGroup = "fast"

// placementGrouped is n workers, each carrying the placement-group value pick returns,
// or none when it returns an empty one.
func placementGrouped(n int, pick func(i int) string) []Worker {
	return fleetOf(n, func(i int, w *Worker) {
		if value := pick(i); value != "" {
			w.Kube.Labels = map[string]string{PlacementGroupLabel: value}
		}
	})
}

// splitWithFindings runs the role split and the size split, keeping the
// findings.
func splitWithFindings(workers []Worker) ([]simplyblockv1alpha2.NodeSet, []Finding) {
	grouper := GroupByHardware{}
	return splitMembers(SplitByRole{}.Build(grouper.Group(workers)), workers, grouper, 1)
}

// findingIn is the first finding under the reason, or none.
func findingIn(findings []Finding, reason string) *Finding {
	for i := range findings {
		if findings[i].Reason == reason {
			return &findings[i]
		}
	}
	return nil
}

// Every worker labeled: one node set per value, named by it, and no split.
func TestPlacementGroupsFormTheNodeSets(t *testing.T) {
	sets, found := splitWithFindings(placementGrouped(40, func(i int) string {
		if i < 10 {
			return fastGroup
		}
		return "dense"
	}))
	got := sizes(sets)
	if got[fastGroup] != 10 || got["dense"] != 30 || len(got) != 2 {
		t.Errorf("sets = %v, want fast of 10 and dense of 30", got)
	}
	if len(found) != 0 {
		t.Errorf("findings = %+v, want none", found)
	}
}

// Some workers labeled: the unlabeled ones are split as usual, and named.
func TestUnlabeledWorkersBesidePlacementGroupsAreSplitAndReported(t *testing.T) {
	sets, found := splitWithFindings(placementGrouped(6, func(i int) string {
		if i < 4 {
			return fastGroup
		}
		return ""
	}))
	got := sizes(sets)
	if got[fastGroup] != 4 || got[DefaultNodeSetName+"-1"] != 2 || len(got) != 2 {
		t.Errorf("sets = %v, want fast of 4 and %s-1 of 2", got, DefaultNodeSetName)
	}
	incomplete := findingIn(found, PlacementGroupLabelIncomplete)
	if incomplete == nil || !strings.Contains(incomplete.Message, "worker-004") ||
		!strings.Contains(incomplete.Message, "worker-005") || strings.Contains(incomplete.Message, "worker-000") {
		t.Errorf("%s = %+v, want it to name worker-004 and worker-005 alone", PlacementGroupLabelIncomplete, incomplete)
	}
}

// A value whose workers are in two regions cannot be one member: it is
// reported, and the node sets it makes are still named apart.
func TestAPlacementGroupSpanningRegionsIsReported(t *testing.T) {
	workers := placementGrouped(4, func(int) string { return fastGroup })
	for i := range workers {
		region := "eu-west-1"
		if i%2 == 1 {
			region = "us-east-1"
		}
		workers[i].Kube.Labels[corev1.LabelTopologyRegion] = region
	}
	sets, found := splitWithFindings(workers)

	invalid := findingIn(found, PlacementGroupInvalid)
	if invalid == nil || !strings.Contains(invalid.Message, fastGroup) {
		t.Errorf("%s = %+v, want it to name the value fast", PlacementGroupInvalid, invalid)
	}
	seen := map[string]bool{}
	for _, set := range sets {
		if seen[set.Name] {
			t.Errorf("two node sets are named %s", set.Name)
		}
		seen[set.Name] = true
	}
}

// A value that cannot be a node set name is reported naming its workers, and
// they are split as unlabeled workers are.
func TestAPlacementGroupThatIsNotANameIsReported(t *testing.T) {
	sets, found := splitWithFindings(placementGrouped(4, func(i int) string {
		if i < 2 {
			return "Not_A-Name"
		}
		return fastGroup
	}))
	invalid := findingIn(found, PlacementGroupInvalid)
	if invalid == nil || !strings.Contains(invalid.Message, "worker-000") || !strings.Contains(invalid.Message, "worker-001") {
		t.Errorf("%s = %+v, want it to name worker-000 and worker-001", PlacementGroupInvalid, invalid)
	}
	for _, set := range sets {
		if set.Name == "Not_A-Name" {
			t.Error("a node set is named after the invalid value")
		}
	}
}

// A labeled node set over the storage-node limit is drafted as it is, for
// approval to refuse with MemberTooLarge.
func TestALargePlacementGroupIsNotSplit(t *testing.T) {
	sets, _ := splitWithFindings(placementGrouped(40, func(int) string { return "big" }))
	if got := sizes(sets); got["big"] != 40 || len(got) != 1 {
		t.Errorf("sets = %v, want big of 40", got)
	}
}
