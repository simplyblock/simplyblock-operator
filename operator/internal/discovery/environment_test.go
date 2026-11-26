// What a draft states about a worker that runs on another provider's
// Kubernetes than the cluster's control plane, such as an EKS or an OKE node
// group joined to a GKE cluster through Google Anthos.

package discovery

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/inventory"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// providerLabels are the node-pool labels each managed service puts on the
// nodes it provisions.
var providerLabels = map[simplyblockv1alpha2.KubernetesEnvironment]map[string]string{
	simplyblockv1alpha2.KubernetesEnvironmentGKE: {"cloud.google.com/gke-nodepool": "storage"},
	simplyblockv1alpha2.KubernetesEnvironmentEKS: {"eks.amazonaws.com/nodegroup": "storage"},
	simplyblockv1alpha2.KubernetesEnvironmentOKE: {"oke.oraclecloud.com/pool.name": "storage"},
}

// providerPlan plans a uniform fleet whose workers worker-1, worker-2, and so on
// were built by the providers given, in that order, under a cluster concluded
// to be the environment given. An empty provider is a node with no markers.
func providerPlan(
	cluster simplyblockv1alpha2.KubernetesEnvironment, providers ...simplyblockv1alpha2.KubernetesEnvironment,
) Plan {
	nodes := make([]corev1.Node, 0, len(providers))
	for i, provider := range providers {
		nodes = append(nodes, withLabels(fmt.Sprintf("worker-%d", i+1), providerLabels[provider]))
	}
	planner := Planner{Environment: cluster, KubeNodes: KubeNodesOf(nodes)}
	return planner.Plan(uniformFleet(len(providers)), nil)
}

// environmentsByWorker maps every drafted worker to its group's environment.
func environmentsByWorker(plan Plan) map[string]simplyblockv1alpha2.KubernetesEnvironment {
	out := map[string]simplyblockv1alpha2.KubernetesEnvironment{}
	for _, set := range plan.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				out[worker] = group.Environment
			}
		}
	}
	return out
}

func TestAWorkerOnAnotherProviderStatesItsOwnEnvironment(t *testing.T) {
	plan := providerPlan(simplyblockv1alpha2.KubernetesEnvironmentGKE,
		simplyblockv1alpha2.KubernetesEnvironmentGKE,
		simplyblockv1alpha2.KubernetesEnvironmentEKS,
		simplyblockv1alpha2.KubernetesEnvironmentOKE)

	got := environmentsByWorker(plan)
	want := map[string]simplyblockv1alpha2.KubernetesEnvironment{
		// The control plane's own workers are on the document's environment,
		// which their group leaves unstated.
		"worker-1": "",
		"worker-2": simplyblockv1alpha2.KubernetesEnvironmentEKS,
		"worker-3": simplyblockv1alpha2.KubernetesEnvironmentOKE,
	}
	for worker, env := range want {
		if got[worker] != env {
			t.Errorf("%s is drafted with environment %q, want %q", worker, got[worker], env)
		}
	}
	// A group states one environment for every worker it lists, so identical
	// hardware on three providers is three groups.
	if n := groupCount(plan); n != 3 {
		t.Errorf("the draft has %d group(s), want one per provider", n)
	}
}

func TestTheDraftNamesTheWorkersOnAnotherProvider(t *testing.T) {
	plan := providerPlan(simplyblockv1alpha2.KubernetesEnvironmentGKE,
		simplyblockv1alpha2.KubernetesEnvironmentGKE,
		simplyblockv1alpha2.KubernetesEnvironmentEKS)

	finding := findingUnder(plan, WorkerEnvironmentDiffers)
	if finding == nil {
		t.Fatalf("the plan raised no %s finding: %+v", WorkerEnvironmentDiffers, plan.Findings)
	}
	for _, word := range []string{"worker-2", "EKS", "GKE"} {
		if !strings.Contains(finding.Message, word) {
			t.Errorf("the finding does not name %s: %s", word, finding.Message)
		}
	}
}

// A fleet on one provider is the common case and states nothing per group.
func TestAFleetOnOneProviderStatesNoGroupEnvironment(t *testing.T) {
	plan := providerPlan(simplyblockv1alpha2.KubernetesEnvironmentEKS,
		simplyblockv1alpha2.KubernetesEnvironmentEKS,
		simplyblockv1alpha2.KubernetesEnvironmentEKS)

	for worker, env := range environmentsByWorker(plan) {
		if env != "" {
			t.Errorf("%s is drafted with environment %q in a fleet on one provider", worker, env)
		}
	}
	if finding := findingUnder(plan, WorkerEnvironmentDiffers); finding != nil {
		t.Errorf("a fleet on one provider raised %s: %s", finding.Reason, finding.Message)
	}
}

// A node without a marker says nothing about itself, so it stays on the
// cluster's environment rather than being drafted as vanilla Kubernetes.
func TestAWorkerWithNoMarkersStaysOnTheClustersEnvironment(t *testing.T) {
	plan := providerPlan(simplyblockv1alpha2.KubernetesEnvironmentGKE, "", "")

	for worker, env := range environmentsByWorker(plan) {
		if env != "" {
			t.Errorf("%s carries no marker and is drafted with environment %q", worker, env)
		}
	}
}

func TestKubeNodeOfReadsTheNodesOwnDistribution(t *testing.T) {
	eks := KubeNodeOf(withLabels("worker-1", providerLabels[simplyblockv1alpha2.KubernetesEnvironmentEKS]))
	if eks.Distribution != inventory.DistributionEKS {
		t.Errorf("an EKS node reads as %q, want %q", eks.Distribution, inventory.DistributionEKS)
	}
	if plain := KubeNodeOf(kubeWorker("worker-2")); plain.Distribution != "" {
		t.Errorf("a node with no markers reads as %q, want none", plain.Distribution)
	}
}
