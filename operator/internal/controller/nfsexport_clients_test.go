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
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
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

// Regression: pnfs-1791981266 on OpenShift. OVN-Kubernetes leaves spec.podCIDRs
// empty and records each node's pod subnet in its k8s.ovn.org/node-subnets
// annotation, and a node reaches the metadata server from its address there.
// With the InternalIPs alone, the server refused every mount with an access
// denial. The subnet in the annotation is admitted as spec.podCIDRs would be.
func TestClientSetAdmitsTheOVNKubernetesNodeSubnet(t *testing.T) {
	ovn := kubeNode("worker-4", "192.168.10.247")
	ovn.Annotations = map[string]string{"k8s.ovn.org/node-subnets": `{"default":["10.129.2.0/23"]}`}
	r, _ := newExportReconciler(t, &fakeAssembler{}, readyExport(), ovn)

	got, err := r.clusterNodeAddresses(context.Background())
	if err != nil {
		t.Fatalf("clusterNodeAddresses: %v", err)
	}
	slices.Sort(got)
	if want := []string{"10.129.2.0/23", "100.64.0.0/16", "192.168.10.247"}; !slices.Equal(got, want) {
		t.Errorf("clients = %v, want %v", got, want)
	}
}

// spec.podCIDRs, where the CNI fills it, stays the source: the annotation is
// read only on a node that has none.
func TestClientSetPrefersSpecPodCIDRsOverTheOVNAnnotation(t *testing.T) {
	node := kubeNode("vm01", testNodeIP)
	node.Spec.PodCIDRs = []string{"10.244.1.0/24"}
	node.Annotations = map[string]string{"k8s.ovn.org/node-subnets": `{"default":["10.129.2.0/23"]}`}
	r, _ := newExportReconciler(t, &fakeAssembler{}, readyExport(), node)

	got, err := r.clusterNodeAddresses(context.Background())
	if err != nil {
		t.Fatalf("clusterNodeAddresses: %v", err)
	}
	slices.Sort(got)
	if want := []string{"10.244.1.0/24", "100.64.0.0/16", testNodeIP}; !slices.Equal(got, want) {
		t.Errorf("clients = %v, want %v", got, want)
	}
}

// An annotation that does not parse admits no subnet of its own: the set never
// widens on an input it cannot read. The node is still an OVN-Kubernetes node,
// so the join subnet is admitted.
func TestClientSetIgnoresAMalformedOVNAnnotation(t *testing.T) {
	node := kubeNode("vm01", testNodeIP)
	node.Annotations = map[string]string{"k8s.ovn.org/node-subnets": `{"default":"*"}`}
	r, _ := newExportReconciler(t, &fakeAssembler{}, readyExport(), node)

	got, err := r.clusterNodeAddresses(context.Background())
	if err != nil {
		t.Fatalf("clusterNodeAddresses: %v", err)
	}
	slices.Sort(got)
	if want := []string{"100.64.0.0/16", testNodeIP}; !slices.Equal(got, want) {
		t.Errorf("clients = %v, want %v", got, want)
	}
}

// ovnNode is a node of a cluster running OVN-Kubernetes, which records its pod
// subnet in an annotation rather than in spec.podCIDRs.
func ovnNode(name, internalIP, subnet string) *corev1.Node {
	node := kubeNode(name, internalIP)
	node.Annotations = map[string]string{"k8s.ovn.org/node-subnets": `{"default":["` + subnet + `"]}`}
	return node
}

// exportReconcilerWithOpenShiftNetwork is newExportReconciler on a cluster that
// serves OpenShift's network operator config, with network as its object.
func exportReconcilerWithOpenShiftNetwork(
	t *testing.T, network *unstructured.Unstructured, objects ...client.Object,
) *NFSExportReconciler {
	t.Helper()
	scheme := newTestScheme(t, corev1.AddToScheme, discoveryv1.AddToScheme)
	scheme.AddKnownTypeWithName(openShiftNetworkGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(openShiftNetworkGVK.GroupVersion().WithKind("NetworkList"), &unstructured.UnstructuredList{})
	cl := newTestClient(t, scheme, []client.Object{&simplyblockv1alpha2.NFSExport{}}, append(objects, network)...)
	return &NFSExportReconciler{Client: cl, Scheme: scheme, Recorder: &fakeRecorder{}, OperatorNamespace: testOperatorNS}
}

// Regression: pnfs-1791984348 on OpenShift. A node plugin mounts from the
// host network through the export's ClusterIP, and OVN-Kubernetes' gateway
// router rewrites that traffic to the node's address on the join subnet, so
// the guest saw 100.64.0.5, .7 and .8 and refused every mount. On a cluster
// running OVN-Kubernetes without OpenShift's network config, the join subnet
// is OVN's default.
func TestClientSetAdmitsTheOVNJoinSubnet(t *testing.T) {
	r, _ := newExportReconciler(t, &fakeAssembler{}, readyExport(),
		ovnNode("worker-1", "192.168.10.244", "10.131.0.0/23"))

	got, err := r.clusterNodeAddresses(context.Background())
	if err != nil {
		t.Fatalf("clusterNodeAddresses: %v", err)
	}
	if !slices.Contains(got, "100.64.0.0/16") {
		t.Errorf("clients = %v, want OVN's default join subnet 100.64.0.0/16", got)
	}
}

// On OpenShift the join subnet is the one the network operator configures.
func TestClientSetAdmitsTheConfiguredOpenShiftJoinSubnet(t *testing.T) {
	network := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "cluster"},
		"spec": map[string]any{"defaultNetwork": map[string]any{"ovnKubernetesConfig": map[string]any{
			"ipv4": map[string]any{"internalJoinSubnet": "100.66.0.0/16"},
		}}},
	}}
	network.SetGroupVersionKind(openShiftNetworkGVK)
	r := exportReconcilerWithOpenShiftNetwork(t, network, ovnNode("worker-1", "192.168.10.244", "10.131.0.0/23"))

	got, err := r.clusterNodeAddresses(context.Background())
	if err != nil {
		t.Fatalf("clusterNodeAddresses: %v", err)
	}
	if !slices.Contains(got, "100.66.0.0/16") || slices.Contains(got, "100.64.0.0/16") {
		t.Errorf("clients = %v, want the configured join subnet 100.66.0.0/16 and not the default", got)
	}
}

// A cluster without OVN-Kubernetes, such as Talos or K3s with Flannel, has no
// join subnet, and its client set stays the nodes and their pod subnets.
func TestClientSetHasNoJoinSubnetWithoutOVN(t *testing.T) {
	node := kubeNode("vm01", testNodeIP)
	node.Spec.PodCIDRs = []string{"10.244.1.0/24"}
	r, _ := newExportReconciler(t, &fakeAssembler{}, readyExport(), node)

	got, err := r.clusterNodeAddresses(context.Background())
	if err != nil {
		t.Fatalf("clusterNodeAddresses: %v", err)
	}
	slices.Sort(got)
	if want := []string{"10.244.1.0/24", testNodeIP}; !slices.Equal(got, want) {
		t.Errorf("clients = %v, want %v", got, want)
	}
}
