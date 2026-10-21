// Which hosts may mount an export.
//
// The set goes verbatim into an exports(5) entry, so it is the only thing
// between a shared filesystem and every host that can reach the metadata
// server. Nothing widens on a missing input: an empty set is a visible wait,
// while a wildcard fallback is a hole nobody notices.
//
// Derived every pass rather than stored, because it changes as nodes come and
// go and it belongs in status. Specified by design-pnfs-rwx.md §7.1.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The OpenShift network operator config, read for the OVN join subnet. A
// cluster without the API answers with no such kind, which is not an error.
// +kubebuilder:rbac:groups=operator.openshift.io,resources=networks,verbs=get

// clusterNodeAddresses is every node that could run a pod using this volume.
//
// Wider than the design's "nodes currently running a consuming pod" and
// narrower than everything. Narrowing to actual placement means rewriting the
// entry as pods move, and an entry rewritten under a live mount is a client
// that loses its export mid-write.
//
// Each node's pod CIDR is admitted beside its InternalIP. A node reaches the
// metadata server pod across the pod network, and the CNI rewrites the source
// to the node's own address in its pod CIDR (a tunnel or bridge address, which
// depends on the CNI and on whether the two share a node), so the guest may
// never see the InternalIP (design-pnfs-mds-vm.md §8.3). The CIDR also admits
// the node's pods, which is why which client may move data through the
// metadata server is enforced on the client, not here.
func (r *NFSExportReconciler) clusterNodeAddresses(ctx context.Context) ([]string, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing cluster nodes for the export's client set: %w", err)
	}
	clients := make([]string, 0, len(nodes.Items))
	ovn := false
	for i := range nodes.Items {
		if addr := internalAddress(&nodes.Items[i]); addr != "" {
			clients = append(clients, addr)
		}
		clients = append(clients, podCIDRs(&nodes.Items[i])...)
		_, annotated := nodes.Items[i].Annotations[ovnNodeSubnetsAnnotation]
		ovn = ovn || annotated
	}
	if ovn {
		join, err := r.ovnJoinSubnet(ctx)
		if err != nil {
			return nil, err
		}
		clients = append(clients, join)
	}
	return clients, nil
}

// ovnDefaultJoinSubnet is the join subnet OVN-Kubernetes uses when nothing
// configures another.
const ovnDefaultJoinSubnet = "100.64.0.0/16"

// openShiftNetworkGVK is OpenShift's network operator config, read unstructured
// so the operator does not depend on OpenShift's API module for one field.
var openShiftNetworkGVK = schema.GroupVersionKind{Group: "operator.openshift.io", Version: "v1", Kind: "Network"}

// ovnJoinSubnet is the subnet OVN-Kubernetes' gateway routers take their
// addresses from. A node plugin mounts from the host network through the
// export's ClusterIP, and on OVN-Kubernetes the gateway router rewrites that
// traffic to the node's address on this subnet, so the guest sees neither the
// InternalIP nor the pod subnet. Only the gateway routers hold addresses on
// it, so admitting it admits the nodes' host traffic and no pod.
//
// OpenShift configures it in its network operator config, as
// ipv4.internalJoinSubnet or, in older releases, v4InternalSubnet. A cluster
// without that API, or one that sets neither, uses OVN's default.
func (r *NFSExportReconciler) ovnJoinSubnet(ctx context.Context) (string, error) {
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(openShiftNetworkGVK)
	err := r.Get(ctx, client.ObjectKey{Name: "cluster"}, network)
	switch {
	case meta.IsNoMatchError(err), runtime.IsNotRegisteredError(err), apierrors.IsNotFound(err):
		return ovnDefaultJoinSubnet, nil
	case err != nil:
		return "", fmt.Errorf("reading the OpenShift network config: %w", err)
	}
	config := []string{"spec", "defaultNetwork", "ovnKubernetesConfig"}
	for _, path := range [][]string{
		append(slices.Clone(config), "ipv4", "internalJoinSubnet"),
		append(slices.Clone(config), "v4InternalSubnet"),
	} {
		if subnet, _, _ := unstructured.NestedString(network.Object, path...); subnet != "" {
			if _, err := netip.ParsePrefix(subnet); err == nil {
				return subnet, nil
			}
		}
	}
	return ovnDefaultJoinSubnet, nil
}

// ovnNodeSubnetsAnnotation is where OVN-Kubernetes, OpenShift's CNI, records a
// node's pod subnets, which it does not write into spec.podCIDRs.
const ovnNodeSubnetsAnnotation = "k8s.ovn.org/node-subnets"

// podCIDRs is the node's pod network, both families when it has two: its
// spec.podCIDRs, or, where the CNI leaves those empty, the subnets
// OVN-Kubernetes records in the node's annotation.
func podCIDRs(node *corev1.Node) []string {
	if len(node.Spec.PodCIDRs) > 0 {
		return node.Spec.PodCIDRs
	}
	if node.Spec.PodCIDR != "" {
		return []string{node.Spec.PodCIDR}
	}
	return ovnNodeSubnets(node)
}

// ovnNodeSubnets reads the default network's subnets from the OVN-Kubernetes
// annotation, which holds a list, or a single string in older releases.
// Anything that is not a CIDR is left out, so the client set never widens on a
// value it cannot read.
func ovnNodeSubnets(node *corev1.Node) []string {
	raw, found := node.Annotations[ovnNodeSubnetsAnnotation]
	if !found {
		return nil
	}
	var networks map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &networks); err != nil {
		return nil
	}
	var subnets []string
	if err := json.Unmarshal(networks["default"], &subnets); err != nil {
		var single string
		if err := json.Unmarshal(networks["default"], &single); err != nil {
			return nil
		}
		subnets = []string{single}
	}
	valid := make([]string, 0, len(subnets))
	for _, subnet := range subnets {
		if _, err := netip.ParsePrefix(subnet); err == nil {
			valid = append(valid, subnet)
		}
	}
	return valid
}

// internalAddress is the one address type a client can reach the node at.
func internalAddress(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address
		}
	}
	return ""
}
