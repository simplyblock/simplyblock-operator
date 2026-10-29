// What the reconcile does about a node whose SPDK pod holds an address other than
// the declared one: its listeners were created on an address nothing can be
// assumed to answer, so the node is taken out of service and left there until
// the declaration or the definition is corrected.

package node

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// I-33
func TestAMismatchedNodeIsShutDownThroughTheControlPlane(t *testing.T) {
	r, _ := onANetwork(t, onDefinition("storage-a"), anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.77")))
	api := r.API.(*scriptedControlPlane)

	settle(t, r)

	if got := api.asked("ShutdownNode"); got != 1 {
		t.Fatalf("ShutdownNode was asked %d times, want 1 (calls: %v)", got, api.calls)
	}
	if api.calls[len(api.calls)-1] != "ShutdownNode:"+opsNodeID {
		t.Fatalf("the shutdown named %v", api.calls)
	}
}

// I-34
func TestAMismatchedNodeStaysShutDownAndIsNeverRestarted(t *testing.T) {
	r, _ := onANetwork(t, onDefinition("storage-a"), anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.77")))
	api := r.API.(*scriptedControlPlane)

	settle(t, r)
	api.reporting(nodeStatusOffline)
	for range 3 {
		settle(t, r)
	}

	if got := api.asked("ShutdownNode"); got != 1 {
		t.Errorf("an offline node was shut down again: %d calls", got)
	}
	if got := api.asked("RestartNode"); got != 0 {
		t.Errorf("a mismatched node was restarted: %d calls", got)
	}
}

func TestAnInShutdownNodeIsNotShutDownAgain(t *testing.T) {
	r, _ := onANetwork(t, onDefinition("storage-a"), anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.77")))
	api := r.API.(*scriptedControlPlane)
	api.reporting(nodeStatusInShutdown)

	settle(t, r)

	if got := api.asked("ShutdownNode"); got != 0 {
		t.Fatalf("a node already in shutdown was shut down again: %d calls", got)
	}
}

func TestAnAttachedOrPendingNodeIsNeverShutDown(t *testing.T) {
	for name, pod := range map[string]*corev1.Pod{
		"attached":    anSPDKPodHolding(corev1.PodRunning, attachedAs("10.10.10.101")),
		"not running": anSPDKPodHolding(corev1.PodPending, ""),
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := onANetwork(t, onDefinition("storage-a"), pod)

			settle(t, r)
			settle(t, r)

			if got := r.API.(*scriptedControlPlane).asked("ShutdownNode"); got != 0 {
				t.Fatalf("ShutdownNode was asked %d times", got)
			}
		})
	}
}
