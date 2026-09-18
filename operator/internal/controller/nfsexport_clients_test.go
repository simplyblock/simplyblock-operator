// Resolving spec.clientPolicy into the effective client set.
//
// The set is what goes into the exports(5) entry, so it is the only thing
// standing between a shared filesystem and every host that can reach the
// metadata server. Each mode is asserted with its negative: what it lets in,
// and what it keeps out.

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
// one a NodeScoped policy resolves to and the one an export is reachable at.
const testNodeIP = "192.168.10.81"

func kubeNode(name, internalIP string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: internalIP},
		}},
	}
}

// NodeScoped is the default, and the default has to be safe: a policy nobody
// wrote must not publish the filesystem to the whole network. Every node that
// can run a pod is in the set, which is wider than the nodes currently running
// one and narrower than everything -- the narrowing to actual placement is the
// refinement noted in the design, and until it exists this is the floor.
func TestNodeScopedAllowsClusterNodesAndNothingElse(t *testing.T) {
	r, _ := newExportReconciler(t, &fakeAssembler{}, readyExport(),
		kubeNode("vm01", testNodeIP), kubeNode("vm02", "192.168.10.82"))

	got, err := r.allowedClients(context.Background(), readyExport())
	if err != nil {
		t.Fatalf("allowedClients: %v", err)
	}
	slices.Sort(got)
	want := []string{testNodeIP, "192.168.10.82"}
	if !slices.Equal(got, want) {
		t.Errorf("clients = %v, want %v", got, want)
	}
	if slices.Contains(got, "*") {
		t.Error("NodeScoped resolved to a wildcard")
	}
}

// Subnet takes exactly what was written, and does not quietly add the cluster's
// own nodes to it: a policy that says one CIDR means that CIDR.
func TestSubnetTakesTheNamedCIDRsOnly(t *testing.T) {
	e := readyExport()
	e.Spec.ClientPolicy = &simplyblockv1alpha2.NFSExportClientPolicy{
		Mode:    simplyblockv1alpha2.NFSExportClientPolicyModeSubnet,
		Subnets: []string{"10.42.0.0/16"},
	}
	r, _ := newExportReconciler(t, &fakeAssembler{}, e, kubeNode("vm01", testNodeIP))

	got, err := r.allowedClients(context.Background(), e)
	if err != nil {
		t.Fatalf("allowedClients: %v", err)
	}
	if !slices.Equal(got, []string{"10.42.0.0/16"}) {
		t.Errorf("clients = %v, want only the named CIDR", got)
	}
}

// Open is the wildcard, and it is only ever reached by asking for it.
func TestOpenIsTheOnlyWildcard(t *testing.T) {
	e := readyExport()
	e.Spec.ClientPolicy = &simplyblockv1alpha2.NFSExportClientPolicy{
		Mode: simplyblockv1alpha2.NFSExportClientPolicyModeOpen,
	}
	r, _ := newExportReconciler(t, &fakeAssembler{}, e)

	got, err := r.allowedClients(context.Background(), e)
	if err != nil {
		t.Fatalf("allowedClients: %v", err)
	}
	if !slices.Equal(got, []string{"*"}) {
		t.Errorf("clients = %v, want the wildcard", got)
	}
}

// A Subnet policy with no subnets resolves to nothing rather than to
// everything. An export to nobody is a visible wait; an export to everyone is a
// silent hole.
func TestSubnetWithNoSubnetsResolvesToNothing(t *testing.T) {
	e := readyExport()
	e.Spec.ClientPolicy = &simplyblockv1alpha2.NFSExportClientPolicy{
		Mode: simplyblockv1alpha2.NFSExportClientPolicyModeSubnet,
	}
	r, _ := newExportReconciler(t, &fakeAssembler{}, e, kubeNode("vm01", testNodeIP))

	got, err := r.allowedClients(context.Background(), e)
	if err != nil {
		t.Fatalf("allowedClients: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("clients = %v, want none", got)
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
		testNode(testMDSHost, nil), kubeNode(workerNodeFor(testMDSHost), "192.168.10.83"))

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
		testNode(testMDSHost, nil),
		kubeNode(workerNodeFor(testMDSHost), "192.168.10.83"))

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
	r, cl := newExportReconciler(t, &fakeAssembler{}, e,
		testNode(testMDSHost, nil),
		kubeNode(workerNodeFor(testMDSHost), ""))

	reconcileExport(t, r)

	got := loadExport(t, cl)
	if got.Status.Phase == simplyblockv1alpha2.NFSExportPhaseAssembling {
		t.Error("an export bound to a host with no address, so no client can mount it")
	}
}
