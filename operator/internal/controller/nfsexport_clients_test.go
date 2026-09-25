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

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// testNodeIP is the address the baseline Kubernetes node publishes, and so the
// one the client set resolves to and the one an export is reachable at.
const testNodeIP = "192.168.10.81"

// kubeNode is a node an export may be bound to, so it carries the label
// selection requires. An unlabeled one is the subject of its own test.
func kubeNode(name, internalIP string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{MDSCapableLabel: "true"},
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
	e := readyExport()
	e.Status.AllowedClients = nil
	// The node the export binds to has to exist as a Kubernetes node, because
	// the export's address is that node's.
	r, cl := newExportReconciler(t, &fakeAssembler{}, e,
		testNode(testMDSHost, nil))

	reconcileExport(t, r)

	if got := loadExport(t, cl).Status.AllowedClients; len(got) == 0 {
		t.Error("binding an export left its client set empty, so it publishes to nobody")
	}
}

// The address a client mounts. Until failover exists there is no Service to
// front the metadata server, so the record carries the bound host's own
// address: the alternative is an empty field the CSI node path refuses with
// "has no export address yet," which names the symptom and not the cause.
func TestBindingRecordsTheMDSAddress(t *testing.T) {
	e := readyExport()
	e.Status.MDSNodeIP = ""
	r, cl := newExportReconciler(t, &fakeAssembler{}, e,
		testNode(testMDSHost, nil))

	reconcileExport(t, r)

	if got := loadExport(t, cl).Status.MDSNodeIP; got != "192.168.10.83" {
		t.Errorf("mdsNodeIP = %q, want the bound host's address", got)
	}
}

// A bound host with no address is not bound: mounting an empty address fails at
// the client with a message about the mount rather than about the host, so the
// binding waits instead.
func TestBindingWaitsForAHostWithAnAddress(t *testing.T) {
	e := readyExport()
	addressless := testNode(testMDSHost, func(n *corev1.Node) {
		n.Status.Addresses = nil
	})
	r, cl := newExportReconciler(t, &fakeAssembler{}, e, addressless)

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase == simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Error("an export bound to a host with no address, so no client can mount it")
	}
}

// A node that has not been labeled is never picked, however healthy it looks.
//
// The operator cannot see whether a host has nfs-utils, a kernel nfsd, or the
// client-side blkmapd, so the label is how an administrator says which nodes
// are equipped. Binding an unlabeled one would fail in Assembling for a reason
// nobody could guess from the export.
func TestUnlabeledNodesAreNeverBound(t *testing.T) {
	e := readyExport()
	unlabeled := testNode(testMDSHost, func(n *corev1.Node) {
		n.Labels = nil
	})

	r, cl := newExportReconciler(t, &fakeAssembler{}, e, unlabeled)

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.MDSNodeName != "" {
		t.Errorf("bound to %q, which is not labeled %s", got.Status.MDSNodeName, MDSCapableLabel)
	}
	if got.Status.Phase == simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Error("an export began assembling on a host nothing said could serve")
	}
}
