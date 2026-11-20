// What a node's reconcile does with the SPDK pod's network-status: it records
// each network's phase on the node, announces the first full attach once, and
// goes quiet while nothing changes.

package node

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const spdkRPCPort = int32(8080)

// onANetwork declares one data network on the node's cluster, records the address
// the operator assigned the node there, and gives it an RPC port, and returns the reconciler.
func onANetwork(
	t *testing.T, network *simplyblockv1alpha2.StorageNetworkSpec, objects ...client.Object,
) (*StorageNodeReconciler, client.Client) {
	t.Helper()
	r, apiClient := aSteadyNode(t, aControlPlane(), objects...)
	ctx := context.Background()

	cluster := &simplyblockv1alpha2.StorageCluster{}
	if err := apiClient.Get(ctx, client.ObjectKey{Namespace: opsNamespace, Name: opsCluster}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{
		Network: network,
	}
	if err := apiClient.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}

	node := nodeRead(t, apiClient)
	if len(network.Data) > 0 {
		node.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
			Network: network.Data[0].Name(), Phase: simplyblockv1alpha2.NodeNetworkPhasePending, Assigned: "10.10.10.101/24",
		}}
	}
	node.Status.Ports = &simplyblockv1alpha2.StorageNodePorts{Rpc: ptr.To(spdkRPCPort)}
	if err := apiClient.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	return r, apiClient
}

// anSPDKPodHolding is the node's SPDK pod, holding what its network-status says.
// It runs on the node's worker, which is how the reconciler tells it from the
// pods of other workers on the same RPC port.
func anSPDKPodHolding(phase corev1.PodPhase, networkStatus string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "snode-spdk-pod-8080-abc", Namespace: opsNamespace,
			Labels: map[string]string{"app": "spdk-app-8080", "role": "simplyblock-storage-node"},
		},
		Spec:   corev1.PodSpec{NodeName: opsWorker},
		Status: corev1.PodStatus{Phase: phase},
	}
	if networkStatus != "" {
		pod.Annotations = map[string]string{NetworkStatusAnnotation: networkStatus}
	}
	return pod
}

func attachedAs(ip string) string {
	return statusOf(defaultNetworkStatus, dataEntry("simplyblock/storage-a", "data0", ip))
}

func recordedEvents(r *StorageNodeReconciler) []string {
	recorder := r.Recorder.(*events.FakeRecorder)
	var got []string
	for {
		select {
		case e := <-recorder.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func recordedReason(got []string, reason string) int {
	n := 0
	for _, e := range got {
		if strings.Contains(e, reason) {
			n++
		}
	}
	return n
}

// I-32
func TestAnSPDKPodHoldingTheDeclaredAddressIsAttachedAndAnnouncedOnce(t *testing.T) {
	r, apiClient := onANetwork(t, onDefinition("storage-a"), anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.101")))

	settle(t, r)

	got := nodeRead(t, apiClient)
	if len(got.Status.Network) != 1 {
		t.Fatalf("status.network = %+v", got.Status.Network)
	}
	n := got.Status.Network[0]
	if n.Network != "storage-a" || n.Interface != "data0" || n.Address != "10.10.10.101" ||
		n.Phase != simplyblockv1alpha2.NodeNetworkPhaseAttached {
		t.Fatalf("status.network[0] = %+v", n)
	}
	if count := recordedReason(recordedEvents(r), NetworkAttached); count != 1 {
		t.Fatalf("NetworkAttached was recorded %d times, want 1", count)
	}
}

// I-36
func TestAnAttachedNodeReconcilesQuietly(t *testing.T) {
	r, apiClient := onANetwork(t, onDefinition("storage-a"), anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.101")))
	settle(t, r)
	recordedEvents(r)
	before := nodeRead(t, apiClient)

	settle(t, r)

	after := nodeRead(t, apiClient)
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("an attached node was rewritten: %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
	if again := recordedEvents(r); len(again) != 0 {
		t.Errorf("an attached node recorded events: %v", again)
	}
}

// The mismatch is recorded and raised. Taking the node out of service is a
// separate step, so none is asked of the control plane here.
func TestAnSPDKPodHoldingAnotherAddressIsMismatchedAndWarnedOnce(t *testing.T) {
	r, apiClient := onANetwork(t, onDefinition("storage-a"), anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.77")))

	settle(t, r)
	settle(t, r)

	n := nodeRead(t, apiClient).Status.Network
	if len(n) != 1 || n[0].Phase != simplyblockv1alpha2.NodeNetworkPhaseMismatched || n[0].Address != "10.10.10.77" {
		t.Fatalf("status.network = %+v", n)
	}
	if count := recordedReason(recordedEvents(r), NetworkAddressMismatch); count != 1 {
		t.Fatalf("NetworkAddressMismatch was recorded %d times, want 1", count)
	}
}

func TestAnSPDKPodThatIsNotRunningYetIsPending(t *testing.T) {
	r, apiClient := onANetwork(t, onDefinition("storage-a"), anSPDKPodHolding(corev1.PodPending, ""))

	settle(t, r)

	n := nodeRead(t, apiClient).Status.Network
	if len(n) != 1 || n[0].Phase != simplyblockv1alpha2.NodeNetworkPhasePending {
		t.Fatalf("status.network = %+v", n)
	}
	if count := recordedReason(recordedEvents(r), NetworkAttached); count != 0 {
		t.Fatalf("NetworkAttached was recorded for a pod that is not running")
	}
}

func TestANodeWithNoSPDKPodYetIsPending(t *testing.T) {
	r, apiClient := onANetwork(t, onDefinition("storage-a"))

	settle(t, r)

	n := nodeRead(t, apiClient).Status.Network
	if len(n) != 1 || n[0].Phase != simplyblockv1alpha2.NodeNetworkPhasePending {
		t.Fatalf("status.network = %+v", n)
	}
}

// I-37
func TestAHostInterfaceNodeHasNoNetworkStatus(t *testing.T) {
	r, apiClient := onANetwork(t, viaHost(), anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.101")))

	settle(t, r)

	if n := nodeRead(t, apiClient).Status.Network; len(n) != 0 {
		t.Fatalf("a host interface node has status.network = %+v", n)
	}
	if got := recordedEvents(r); recordedReason(got, "Network") != 0 {
		t.Fatalf("a host interface node recorded network events: %v", got)
	}
}

// Another worker's SPDK pod on the same RPC port is not this node's: with only
// that pod present the node has no pod to verify against and stays Pending,
// rather than being judged by a neighbor's address.
func TestAnotherWorkersPodOnTheSamePortIsNotVerifiedAsTheNodes(t *testing.T) {
	other := anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.222"))
	other.Name = "snode-spdk-pod-8080-other"
	other.Spec.NodeName = "worker-2"
	r, apiClient := onANetwork(t, onDefinition("storage-a"), other)

	settle(t, r)

	got := nodeRead(t, apiClient)
	if len(got.Status.Network) != 1 || got.Status.Network[0].Phase != simplyblockv1alpha2.NodeNetworkPhasePending {
		t.Fatalf("status.network = %+v, want Pending: the only pod on the port belongs to another worker", got.Status.Network)
	}
	if count := recordedReason(recordedEvents(r), NetworkAddressMismatch); count != 0 {
		t.Fatalf("NetworkAddressMismatch was recorded %d times against another worker's pod", count)
	}
}
