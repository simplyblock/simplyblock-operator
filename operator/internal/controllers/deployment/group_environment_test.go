// A group's own environment: what it decides about the nodes the group expands
// into, and which environments admission lets a group name.
//
// A cluster whose workers come from more than one provider, such as Google
// Anthos joining EKS and OKE node groups to a GKE control plane, has one
// environment per provider rather than one for the document. The document's
// spec.environment is the control plane's, and a group states its own where it
// differs.

package deployment

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// nodeOfGroup expands the first worker of the document's first group.
func nodeOfGroup(t *testing.T, config *simplyblockv1alpha2.ClusterDeploymentConfig) *simplyblockv1alpha2.StorageNode {
	t.Helper()
	return reconcilerFor(t).buildNode(config, aCluster(nil),
		config.Spec.NodeSets[0], config.Spec.NodeSets[0].Groups[0], "worker-1", 0)
}

func TestAGroupsEnvironmentDecidesItsNodesKubeletConfiguration(t *testing.T) {
	cases := []struct {
		name     string
		document simplyblockv1alpha2.KubernetesEnvironment
		group    simplyblockv1alpha2.KubernetesEnvironment
		want     bool
	}{
		// A self-managed worker joined to a managed control plane keeps a
		// kubelet change, so its nodes may make one.
		{"a self-managed group under GKE", simplyblockv1alpha2.KubernetesEnvironmentGKE,
			simplyblockv1alpha2.KubernetesEnvironmentVanilla, true},
		// A managed node group rebuilds the kubelet configuration of every node
		// it replaces, so its nodes leave it alone.
		{"an EKS group under a self-managed control plane", simplyblockv1alpha2.KubernetesEnvironmentVanilla,
			simplyblockv1alpha2.KubernetesEnvironmentEKS, false},
		{"an OKE group under GKE", simplyblockv1alpha2.KubernetesEnvironmentGKE,
			simplyblockv1alpha2.KubernetesEnvironmentOKE, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config := aDocument(func(d *simplyblockv1alpha2.ClusterDeploymentConfig) {
				d.Spec.Environment = c.document
				d.Spec.NodeSets[0].Groups[0].Environment = c.group
			})

			got := nodeOfGroup(t, config).Spec.Config.EnableKubeletConfiguration
			if got == nil || *got != c.want {
				t.Errorf("the node states enableKubeletConfiguration=%v, want %t", ptr.StringOrDefault(got, "unset"), c.want)
			}
		})
	}
}

// A group that states no environment is on the document's, which the cluster
// already carries, so its nodes state nothing and the fleet's value decides.
func TestAGroupWithNoEnvironmentStatesNoKubeletConfiguration(t *testing.T) {
	config := aDocument(func(d *simplyblockv1alpha2.ClusterDeploymentConfig) {
		d.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentGKE
	})

	if got := nodeOfGroup(t, config).Spec.Config.EnableKubeletConfiguration; got != nil {
		t.Errorf("the node states enableKubeletConfiguration=%t with no group environment", *got)
	}
}

// A document that states the kubelet flag outright has decided it for the whole
// fleet, and an environment is only ever a default for it.
func TestAStatedKubeletFlagBeatsAGroupsEnvironment(t *testing.T) {
	config := aDocument(func(d *simplyblockv1alpha2.ClusterDeploymentConfig) {
		d.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentGKE
		d.Spec.Cluster.EnableKubeletConfiguration = ptr.To(false)
		d.Spec.NodeSets[0].Groups[0].Environment = simplyblockv1alpha2.KubernetesEnvironmentVanilla
	})

	if got := nodeOfGroup(t, config).Spec.Config.EnableKubeletConfiguration; got != nil {
		t.Errorf("the node states enableKubeletConfiguration=%t over the document's stated false", *got)
	}
}

// OpenShift and Talos are what a whole cluster runs: neither joins a worker to a
// control plane of another distribution, so a group naming one is a mistake.
func TestAGroupMayNotNameOpenShiftOrTalos(t *testing.T) {
	for _, env := range []simplyblockv1alpha2.KubernetesEnvironment{
		simplyblockv1alpha2.KubernetesEnvironmentOpenShift,
		simplyblockv1alpha2.KubernetesEnvironmentTalos,
	} {
		err := createDocument(t, "group-env-"+strings.ToLower(string(env)), func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
			c.Spec.NodeSets[0].Groups[0].Environment = env
		})
		if err == nil {
			t.Errorf("a group naming %s was admitted", env)
		}
	}
}

// For the same reason, no worker of an OpenShift or a Talos cluster runs
// anything else.
func TestAGroupMayNotStateAnEnvironmentUnderOpenShift(t *testing.T) {
	err := createDocument(t, "group-env-under-ocp", func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentOpenShift
		c.Spec.NodeSets[0].Groups[0].Environment = simplyblockv1alpha2.KubernetesEnvironmentEKS
	})
	if err == nil {
		t.Error("a group stating EKS in an OpenShift document was admitted")
	}
}

func TestAGroupOnAnotherProviderIsAccepted(t *testing.T) {
	err := createDocument(t, "group-env-eks", func(c *simplyblockv1alpha2.ClusterDeploymentConfig) {
		c.Spec.Environment = simplyblockv1alpha2.KubernetesEnvironmentGKE
		c.Spec.NodeSets[0].Groups[0].Environment = simplyblockv1alpha2.KubernetesEnvironmentEKS
	})
	if err != nil {
		t.Fatalf("an EKS group in a GKE document was refused: %v", err)
	}
}
