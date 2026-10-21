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
