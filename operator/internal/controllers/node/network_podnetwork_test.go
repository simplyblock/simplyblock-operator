// What a node on the pod network reports: the address of the Service in front
// of its SPDK pod, attached once the pod runs behind it, and no probe or
// attachment on the way there.

package node

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const podNetworkServiceAddress = "10.103.252.64"

// theServiceOf is the Service the node agent keeps in front of the node's SPDK
// pod, as the fake API server holds it.
func theServiceOf() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: podNetworkServiceName(spdkRPCPort, opsClusterID), Namespace: opsNamespace,
			Labels: map[string]string{clusterLabel: opsClusterID},
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
	r, _ := onANetwork(t, viaPodNetwork(), anSPDKPodHolding(corev1.PodRunning, ""), theServiceOf())

	settle(t, r)

	got := podNetworkStatusOf(t, r)
	want := simplyblockv1alpha2.NodeNetworkStatus{
		Network: podNetworkName, Interface: "eth0", Address: podNetworkServiceAddress,
		Phase: simplyblockv1alpha2.NodeNetworkPhaseAttached,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status.network[0] = %+v, want %+v", got, want)
	}
}

func TestAPodNetworkNodeIsPendingUntilItsServiceExists(t *testing.T) {
	r, _ := onANetwork(t, viaPodNetwork(), anSPDKPodHolding(corev1.PodRunning, ""))

	settle(t, r)

	got := podNetworkStatusOf(t, r)
	if got.Phase != simplyblockv1alpha2.NodeNetworkPhasePending || got.Address != "" {
		t.Fatalf("status.network[0] = %+v, want Pending with no address", got)
	}
}

func TestAPodNetworkNodeIsUnavailableWhileItsPodDoesNotRun(t *testing.T) {
	r, _ := onANetwork(t, viaPodNetwork(), anSPDKPodHolding(corev1.PodPending, ""), theServiceOf())

	settle(t, r)

	got := podNetworkStatusOf(t, r)
	if got.Phase != simplyblockv1alpha2.NodeNetworkPhaseUnavailable || got.Address != podNetworkServiceAddress {
		t.Fatalf("status.network[0] = %+v, want Unavailable at the Service's address", got)
	}
}

func TestAPodNetworkNodeIsNeverShutDown(t *testing.T) {
	r, _ := onANetwork(t, viaPodNetwork(), anSPDKPodHolding(corev1.PodRunning, ""), theServiceOf())
	api := r.API.(*scriptedControlPlane)

	settle(t, r)
	settle(t, r)

	if got := api.asked("ShutdownNode"); got != 0 {
		t.Fatalf("ShutdownNode was asked %d times", got)
	}
}

// A Service of another cluster whose id shares the short prefix fronts no pod of
// this node: its address is not the node's.
func TestAServiceOfAnotherClusterIsNotReportedAsTheNodes(t *testing.T) {
	foreign := theServiceOf()
	foreign.Labels = map[string]string{clusterLabel: "cluster-1111-of-someone-else"}
	r, _ := onANetwork(t, viaPodNetwork(), anSPDKPodHolding(corev1.PodRunning, ""), foreign)

	settle(t, r)

	got := podNetworkStatusOf(t, r)
	if got.Phase != simplyblockv1alpha2.NodeNetworkPhasePending || got.Address != "" {
		t.Fatalf("status.network[0] = %+v, want Pending with no address", got)
	}
}

func TestAPodNetworkNodeOfAClusterWithoutAUUIDIsPending(t *testing.T) {
	r, apiClient := onANetwork(t, viaPodNetwork(), anSPDKPodHolding(corev1.PodRunning, ""), theServiceOf())
	_, cluster := nodeAndClusterOf(t, r)
	cluster.Status.UUID = ""
	if err := apiClient.Status().Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}

	settle(t, r)

	got := podNetworkStatusOf(t, r)
	if got.Phase != simplyblockv1alpha2.NodeNetworkPhasePending || got.Address != "" {
		t.Fatalf("status.network[0] = %+v, want Pending with no address", got)
	}
}

func TestTheAttachedEventFiresOnceForAPodNetworkNode(t *testing.T) {
	r, _ := onANetwork(t, viaPodNetwork(), anSPDKPodHolding(corev1.PodRunning, ""), theServiceOf())

	settle(t, r)
	settle(t, r)

	attached := 0
	for _, e := range recordedEvents(r) {
		if strings.Contains(e, NetworkAttached) {
			attached++
		}
	}
	if attached != 1 {
		t.Fatalf("the attached event was recorded %d times, want once", attached)
	}
}

// Provisioning a node on the pod network skips the network check: there is no
// definition to require, no address to assign and no probe to run.
func TestAPodNetworkNodeSkipsTheNetworkCheck(t *testing.T) {
	r, _ := aSteadyNode(t, aControlPlane())

	next, done, err := r.checkConfig(context.Background(),
		anUnprovisionedNode(stepCheckingConfig), clusterDeclaring(viaPodNetwork()))

	if err != nil || !done || next != stepAwaitingSlot {
		t.Fatalf("next=%s done=%v err=%v, want AwaitingSlot", next, done, err)
	}
}
