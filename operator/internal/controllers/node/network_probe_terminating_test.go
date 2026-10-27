// A probe pod that is being deleted is never judged. Probe pods stop slowly
// when a device is handed back to the host, and the next pass of a node whose
// address changed finds the old pod under the same name: its network-status
// belongs to the old address and would read as a mismatch against the new one.

package node

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const probeFinalizer = "test.simplyblock.io/hold"

// terminating deletes the pod and leaves it in the API server, as a pod whose
// sandbox is still being torn down is.
func terminating(t *testing.T, apiClient client.Client, pod *corev1.Pod) {
	t.Helper()
	pod.Finalizers = []string{probeFinalizer}
	if err := apiClient.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func released(t *testing.T, apiClient client.Client, pod *corev1.Pod) {
	t.Helper()
	var fresh corev1.Pod
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Finalizers = nil
	if err := apiClient.Update(context.Background(), &fresh); err != nil {
		t.Fatal(err)
	}
}

func assign(t *testing.T, apiClient client.Client, address string) {
	t.Helper()
	node := nodeRead(t, apiClient)
	node.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
		Network: poolNetwork, Phase: simplyblockv1alpha2.NodeNetworkPhasePending, Assigned: address,
	}}
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
}

func TestAGuestProbeThatIsBeingDeletedIsNotJudgedAgainstTheNewAddress(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	finishProbe(t, apiClient, attachedAs("10.10.10.101"), passingResult)
	old, _ := probeOf(t, apiClient)
	terminating(t, apiClient, old)
	assign(t, apiClient, "10.10.10.104/24")

	next, done, err := check(t, r)

	if err != nil || done || next != stepCheckingNetwork {
		t.Fatalf("next=%s done=%v err=%v, want the node to wait without being held", next, done, err)
	}
	status := nodeRead(t, apiClient).Status.Network
	if status[0].Assigned != "10.10.10.104/24" || len(status[0].Rejected) != 0 {
		t.Fatalf("status.network = %+v, want the new address kept and nothing rejected", status)
	}

	released(t, apiClient, old)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	pod, found := probeOf(t, apiClient)
	if !found {
		t.Fatal("no fresh probe was created once the old one was gone")
	}
	var arping string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "ARPING" {
			arping = e.Value
		}
	}
	if !strings.HasPrefix(arping, "data0=10.10.10.104") {
		t.Fatalf("ARPING = %q, want the probe to start with the node's new address", arping)
	}
}

func TestAHostProbeThatIsBeingDeletedIsWaitedFor(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	passTheGuest(t, r, apiClient)
	finishHostProbe(t, apiClient, `{"targets":[{"address":"10.10.10.101","ok":true}]}`)
	host, _ := hostProbeOf(t, apiClient)
	terminating(t, apiClient, host)

	next, done, err := check(t, r)

	if err != nil || done || next != stepCheckingNetwork {
		t.Fatalf("next=%s done=%v err=%v, want the node to wait for the host probe to go", next, done, err)
	}
	if status := nodeRead(t, apiClient).Status.Network; status[0].HostReachable != nil {
		t.Fatalf("status.network = %+v, want nothing recorded from a probe that is going away", status)
	}
}
