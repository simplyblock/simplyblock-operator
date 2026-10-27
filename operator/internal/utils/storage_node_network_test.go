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
		Data: []simplyblockv1alpha2.StorageNetwork{{Name: "fabric"}},
	})

	ds := BuildStorageNodeDaemonSet(cluster, false, false, "", "", "agent:test")

	if ds.Spec.Template.Spec.HostNetwork {
		t.Fatal("the agent kept hostNetwork although spec.storageNodes.network is declared")
	}
}

// I-40
func TestTheAgentKeepsTheHostNetworkWhenNoNetworkIsDeclared(t *testing.T) {
	for name, cluster := range map[string]*simplyblockv1alpha2.StorageCluster{
		"no storageNodes": {ObjectMeta: metav1.ObjectMeta{Name: "c1"}},
		"no network":      clusterWithNetwork(nil),
	} {
		ds := BuildStorageNodeDaemonSet(cluster, false, false, "", "", "agent:test")
		if !ds.Spec.Template.Spec.HostNetwork {
			t.Errorf("%s: the agent lost hostNetwork with no network declared", name)
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
		Data: []simplyblockv1alpha2.StorageNetwork{{Name: "fabric"}},
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
		Data: []simplyblockv1alpha2.StorageNetwork{{Name: "fabric"}},
	})

	podIP, found := envVarOf(cluster, "POD_IP")

	if !found || podIP.ValueFrom == nil || podIP.ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Fatalf("POD_IP = %+v, want the downward API's status.podIP", podIP)
	}
	if _, found := envVarOf(clusterWithNetwork(nil), "POD_IP"); found {
		t.Fatal("a host-network cluster's agent gained POD_IP, which rolls it on upgrade")
	}
}

func TestTheAgentOfAHostNetworkClusterGetsNoHostAddress(t *testing.T) {
	if _, found := envVarOf(clusterWithNetwork(nil), "HOST_IP"); found {
		t.Fatal("a host-network cluster's agent gained HOST_IP, which rolls it on upgrade")
	}
}
