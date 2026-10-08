// Resolving the effective client set, and the address a client mounts at.
//
// The set is what goes into the exports(5) entry, so it is the only thing
// standing between a shared filesystem and every host that can reach the
// metadata server. It is asserted with its negative: what it lets in, and what
// it keeps out.

package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testNodeIP is the address the baseline Kubernetes node publishes, and so the
// one the client set resolves to and the one an export is reachable at.
const testNodeIP = "192.168.10.81"

// kubeNode is a cluster node a client could mount an export from.
func kubeNode(name, internalIP string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: internalIP},
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// The set has to be safe by construction: nothing publishes the filesystem to
// the whole network. Every node that can run a pod is in it, which is wider
// than the nodes currently running one and narrower than everything -- the
// narrowing to actual placement is the refinement noted in the design, and
// until it exists this is the floor.
func TestClientSetIsClusterNodesAndNothingElse(t *testing.T) {
	r, _ := newExportReconciler(t, &fakeAssembler{}, readyExport(),
		kubeNode("vm01", testNodeIP), kubeNode("vm02", "192.168.10.82"))

	got, err := r.clusterNodeAddresses(context.Background())
	if err != nil {
		t.Fatalf("clusterNodeAddresses: %v", err)
	}
	slices.Sort(got)
	want := []string{testNodeIP, "192.168.10.82"}
	if !slices.Equal(got, want) {
		t.Errorf("clients = %v, want %v", got, want)
	}
	if slices.Contains(got, "*") {
		t.Error("the client set resolved to a wildcard")
	}
}

// The reconciler records the resolved set, because the assembler reads it back
// from status and an export with an empty set publishes to nobody.
func TestBindingRecordsTheResolvedClientSet(t *testing.T) {
	r, cl, _ := newPodHostedReconciler(t, testExport(nil), mdsDriver(), boundMDSPod(readyPod),
		kubeNode("vm01", testNodeIP))

	reconcileExport(t, r)

	if got := loadExport(t, cl).Status.AllowedClients; len(got) == 0 {
		t.Error("binding an export left its client set empty, so it publishes to nobody")
	}
}
