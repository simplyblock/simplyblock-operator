// Tests that the node agent DaemonSet leaves the host network exactly when the
// cluster declares spec.storageNodes.network (storage network design §5.2).
package utils

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func clusterWithNetwork(network *simplyblockv1alpha2.StorageNetworkSpec) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "simplyblock"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			StorageNodes: &simplyblockv1alpha2.StorageNodesSpec{Network: network},
		},
	}
}

// I-39
func TestTheAgentLeavesTheHostNetworkWhenANetworkIsDeclared(t *testing.T) {
	cluster := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
		PodNetwork: true,
	})

	ds := BuildStorageNodeDaemonSet(cluster, false, false, "", "", "agent:test")

	if ds.Spec.Template.Spec.HostNetwork {
		t.Fatal("the agent kept hostNetwork although spec.storageNodes.network is declared")
	}
}

// I-40: the agent is never on the host network. It reads the host's interfaces
// and opens its connections through the host's network namespace instead, which
// it can enter because it shares the host's PID namespace.
func TestTheAgentIsOffTheHostNetworkWhateverTheNetwork(t *testing.T) {
	for name, cluster := range map[string]*simplyblockv1alpha2.StorageCluster{
		"no storageNodes":  {ObjectMeta: metav1.ObjectMeta{Name: "c1"}},
		"no network":       clusterWithNetwork(nil),
		"the host network": clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{SpdkOnHostNetwork: true}),
	} {
		spec := BuildStorageNodeDaemonSet(cluster, false, false, "", "", "agent:test").Spec.Template.Spec
		if spec.HostNetwork {
			t.Errorf("%s: the agent is on the host network", name)
		}
		if !spec.HostPID {
			t.Errorf("%s: the agent has no host PID, so it cannot enter the host's network namespace", name)
		}
	}
}

// envVarOf finds one variable on the agent's container.
func envVarOf(cluster *simplyblockv1alpha2.StorageCluster, name string) (corev1.EnvVar, bool) {
	ds := BuildStorageNodeDaemonSet(cluster, false, false, "", "", "agent:test")
	for _, variable := range ds.Spec.Template.Spec.Containers[0].Env {
		if variable.Name == name {
			return variable, true
		}
	}
	return corev1.EnvVar{}, false
}

// With the agent in the pod network its own interfaces are not the host's, so the
// pod is handed the worker's address. A cluster on the host network gets no new
// variable, because one would roll every agent when the operator is upgraded.
func TestTheAgentOfANetworkClusterIsToldItsHostAddress(t *testing.T) {
	cluster := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
		PodNetwork: true,
	})

	hostIP, found := envVarOf(cluster, "HOST_IP")

	if !found || hostIP.ValueFrom == nil || hostIP.ValueFrom.FieldRef.FieldPath != "status.hostIP" {
		t.Fatalf("HOST_IP = %+v, want the downward API's status.hostIP", hostIP)
	}
}

// The node-preparation job calls the agent, which is no longer at the worker's
// address, so the agent states its own for the job.
func TestTheAgentOfANetworkClusterKnowsItsOwnPodAddress(t *testing.T) {
	cluster := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
		PodNetwork: true,
	})

	podIP, found := envVarOf(cluster, "POD_IP")

	if !found || podIP.ValueFrom == nil || podIP.ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Fatalf("POD_IP = %+v, want the downward API's status.podIP", podIP)
	}
	if _, found := envVarOf(clusterWithNetwork(nil), "POD_IP"); !found {
		t.Fatal("a cluster with no network has an agent with no POD_IP, and the agent is in the pod network")
	}
}

func TestTheAgentOfAClusterWithNoNetworkIsToldItsHostAddress(t *testing.T) {
	if _, found := envVarOf(clusterWithNetwork(nil), "HOST_IP"); !found {
		t.Fatal("the agent has no HOST_IP, and it is in the pod network")
	}
}
