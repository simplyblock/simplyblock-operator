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
		kubeNode("vm01", "192.168.10.81"), kubeNode("vm02", "192.168.10.82"))

	got, err := r.allowedClients(context.Background(), readyExport())
	if err != nil {
		t.Fatalf("allowedClients: %v", err)
	}
	slices.Sort(got)
	want := []string{"192.168.10.81", "192.168.10.82"}
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
	r, _ := newExportReconciler(t, &fakeAssembler{}, e, kubeNode("vm01", "192.168.10.81"))

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
	r, _ := newExportReconciler(t, &fakeAssembler{}, e, kubeNode("vm01", "192.168.10.81"))

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
	r, cl := newExportReconciler(t, &fakeAssembler{}, e,
		testNode(testMDSHost, nil), kubeNode("vm01", "192.168.10.81"))

	reconcileExport(t, r)

	if got := loadExport(t, cl).Status.AllowedClients; len(got) == 0 {
		t.Error("binding an export left its client set empty, so it publishes to nobody")
	}
}
