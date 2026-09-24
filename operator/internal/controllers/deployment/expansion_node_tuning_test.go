// The node-tuning fields a document can state, and the environment shorthand
// they have to survive.
//
// Environment resolves the CPU flags for the environments that have an answer,
// which is most deployments and is why it exists. These tests are about the rest:
// a document that states one of them has to keep it, and a document that states
// nothing has to keep the environment's answer. Both directions matter, because
// an override that the shorthand silently reverses is a field that validates,
// round trips and changes nothing.

package deployment

import (
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The reserved CPU set reaches the cluster the document creates.
//
// Nothing defaults it and the environment shorthand does not reach it, so before
// the document could state it there was no way to reserve CPUs for system
// workloads short of patching the StorageCluster between its creation and the
// first node add.
func TestTheReservedCPUsReachTheCluster(t *testing.T) {
	for _, stated := range []string{"0,1", "0-3", ""} {
		config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.Cluster.ReservedSystemCPU = stated
		})
		r := reconcilerFor(t)

		if got := r.buildWorkload(config).ReservedSystemCPU; got != stated {
			t.Errorf("the cluster reserved %q, want the document's %q", got, stated)
		}
	}
}

// A stated CPU-topology decision survives the environment switch.
//
// OpenShift is the case that matters: its arm of the switch turns topology on,
// so an assignment there that does not look first reads the document's value and
// throws it away. A deployment that asked for topology off would get it on, and
// nothing would say so.
func TestAStatedTopologyDecisionSurvivesTheEnvironment(t *testing.T) {
	for _, environment := range []simplyblockv1alpha2.KubernetesEnvironment{
		simplyblockv1alpha2.KubernetesEnvironmentOpenShift,
		simplyblockv1alpha2.KubernetesEnvironmentVanilla,
	} {
		for _, stated := range []bool{true, false} {
			config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
				c.Spec.Environment = environment
				c.Spec.Cluster.EnableCpuTopology = ptr.To(stated)
			})
			r := reconcilerFor(t)

			got := r.buildWorkload(config).EnableCpuTopology
			if got == nil {
				t.Errorf("%s: a document that said %v produced nothing", environment, stated)
				continue
			}
			if *got != stated {
				t.Errorf("%s: the cluster got topology %v, want the document's %v",
					environment, *got, stated)
			}
		}
	}
}

// A stated kubelet-configuration decision survives the environment switch, for
// the same reason: every arm of it states this flag.
func TestAStatedKubeletDecisionSurvivesTheEnvironment(t *testing.T) {
	for _, environment := range []simplyblockv1alpha2.KubernetesEnvironment{
		simplyblockv1alpha2.KubernetesEnvironmentOpenShift,
		simplyblockv1alpha2.KubernetesEnvironmentTalos,
		simplyblockv1alpha2.KubernetesEnvironmentVanilla,
	} {
		for _, stated := range []bool{true, false} {
			config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
				c.Spec.Environment = environment
				c.Spec.Cluster.EnableKubeletConfiguration = ptr.To(stated)
			})
			r := reconcilerFor(t)

			got := r.buildWorkload(config).EnableKubeletConfiguration
			if got == nil {
				t.Errorf("%s: a document that said %v produced nothing", environment, stated)
				continue
			}
			if *got != stated {
				t.Errorf("%s: the cluster got kubelet configuration %v, want the document's %v",
					environment, *got, stated)
			}
		}
	}
}

// A silent document still gets the environment's answer.
//
// The override is the new half; this is the half that already worked, and the
// one a careless `if` around the switch would take away. Naming OpenShift is
// still meant to decide these without the document repeating them.
func TestASilentDocumentKeepsTheEnvironmentDefaults(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentOpenShift
	})
	r := reconcilerFor(t)

	workload := r.buildWorkload(config)
	if workload.EnableCpuTopology == nil || !*workload.EnableCpuTopology {
		t.Error("an OpenShift document that said nothing lost topology-aware CPU assignment")
	}
	if workload.EnableKubeletConfiguration == nil || !*workload.EnableKubeletConfiguration {
		t.Error("an OpenShift document that said nothing lost its kubelet configuration")
	}
	if workload.OpenShiftCluster == nil || !*workload.OpenShiftCluster {
		t.Error("an OpenShift document did not produce an OpenShift cluster")
	}
}

// Talos says no kubelet configuration, and a silent document keeps that too.
func TestASilentTalosDocumentKeepsItsKubeletAlone(t *testing.T) {
	config := aDocument(func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentTalos
	})
	r := reconcilerFor(t)

	got := r.buildWorkload(config).EnableKubeletConfiguration
	if got == nil || *got {
		t.Errorf("a silent Talos document produced %v, want false", got)
	}
}
