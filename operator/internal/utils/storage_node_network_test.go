// Tests that the node agent DaemonSet is off the host network, with or without
// spec.storageNodes.network declared (storage network design §5.2).
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

// I-40: the agent is on the pod network whether or not a network is declared. A
// declared network adds the attachment; it no longer decides where the agent runs.
func TestTheAgentIsOffTheHostNetworkWhenNoNetworkIsDeclared(t *testing.T) {
	for name, cluster := range map[string]*simplyblockv1alpha2.StorageCluster{
		"no storageNodes": {ObjectMeta: metav1.ObjectMeta{Name: "c1"}},
		"no network":      clusterWithNetwork(nil),
	} {
		ds := BuildStorageNodeDaemonSet(cluster, false, false, "", "", "agent:test")
		if ds.Spec.Template.Spec.HostNetwork {
			t.Errorf("%s: the agent kept hostNetwork", name)
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
// pod is handed the worker's address.
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
	if _, found := envVarOf(clusterWithNetwork(nil), "POD_IP"); !found {
		t.Fatal("a cluster with no network declared has no POD_IP, and its agent is on the pod network too")
	}
}

func TestTheAgentOfAClusterWithNoNetworkIsToldItsHostAddressToo(t *testing.T) {
	if _, found := envVarOf(clusterWithNetwork(nil), "HOST_IP"); !found {
		t.Fatal("a cluster with no network declared has no HOST_IP, and its agent is on the pod network too")
	}
}
