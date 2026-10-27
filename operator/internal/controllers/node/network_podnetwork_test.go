// What a node on the pod network reports: the address of the Service in front
// of its SPDK pod, attached once the pod runs behind it, and no probe or
// attachment on the way there.

package node

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const podNetworkServiceAddress = "10.103.252.64"

// theServiceOf is the Service the node agent keeps in front of the node's SPDK
// pod, as the fake API server holds it.
func theServiceOf(rpcPort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: podNetworkServiceName(rpcPort, opsClusterID), Namespace: opsNamespace,
		},
		Spec: corev1.ServiceSpec{ClusterIP: podNetworkServiceAddress},
	}
}

func podNetworkStatusOf(t *testing.T, r *StorageNodeReconciler) simplyblockv1alpha2.NodeNetworkStatus {
	t.Helper()
	node, _ := nodeAndClusterOf(t, r)
	if len(node.Status.Network) != 1 {
		t.Fatalf("status.network = %+v, want one entry", node.Status.Network)
	}
	return node.Status.Network[0]
}

func TestTheServiceNameFollowsThePodsNaming(t *testing.T) {
	if got := podNetworkServiceName(8080, "15260308-01ab-4256-a01c-8bbf121bbeed"); got != "snode-spdk-svc-8080-152603" {
		t.Fatalf("service name = %q", got)
	}
}

func TestAPodNetworkNodeIsAttachedOnceItsPodRunsBehindTheService(t *testing.T) {
	r, _ := onANetwork(t, viaPodNetwork("cni"), anSPDKPodHolding(corev1.PodRunning, ""), theServiceOf(spdkRPCPort))

	settle(t, r)

	got := podNetworkStatusOf(t, r)
	want := simplyblockv1alpha2.NodeNetworkStatus{
		Network: "cni", Interface: "eth0", Address: podNetworkServiceAddress,
		Phase: simplyblockv1alpha2.NodeNetworkPhaseAttached,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status.network[0] = %+v, want %+v", got, want)
	}
}

func TestAPodNetworkNodeIsPendingUntilItsServiceExists(t *testing.T) {
	r, _ := onANetwork(t, viaPodNetwork("cni"), anSPDKPodHolding(corev1.PodRunning, ""))

	settle(t, r)

	got := podNetworkStatusOf(t, r)
	if got.Phase != simplyblockv1alpha2.NodeNetworkPhasePending || got.Address != "" {
		t.Fatalf("status.network[0] = %+v, want Pending with no address", got)
	}
}

func TestAPodNetworkNodeIsUnavailableWhileItsPodDoesNotRun(t *testing.T) {
	r, _ := onANetwork(t, viaPodNetwork("cni"), anSPDKPodHolding(corev1.PodPending, ""), theServiceOf(spdkRPCPort))

	settle(t, r)

	got := podNetworkStatusOf(t, r)
	if got.Phase != simplyblockv1alpha2.NodeNetworkPhaseUnavailable || got.Address != podNetworkServiceAddress {
		t.Fatalf("status.network[0] = %+v, want Unavailable at the Service's address", got)
	}
}

func TestAPodNetworkNodeIsNeverShutDownOrProbed(t *testing.T) {
	r, apiClient := onANetwork(t, viaPodNetwork("cni"), anSPDKPodHolding(corev1.PodRunning, ""), theServiceOf(spdkRPCPort))
	api := r.API.(*scriptedControlPlane)

	settle(t, r)
	settle(t, r)

	if got := api.asked("ShutdownNode"); got != 0 {
		t.Fatalf("ShutdownNode was asked %d times", got)
	}
	var pods corev1.PodList
	if err := apiClient.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods.Items {
		if pod.Labels["role"] != spdkPodRole {
			t.Fatalf("a probe pod %s was started on the pod network", pod.Name)
		}
	}
}
