// Which IPv4 subnets this node's interfaces are attached to, published as node
// labels the operator places the pNFS metadata server by.
//
// The metadata server reaches the storage nodes over a data network through a
// macvlan on a host interface, so it can only run on a node attached to that
// network, and the macvlan needs the interface's name. The plugin runs on the
// host network of every node, so it reads the host's interfaces directly. It
// lives beside the KVM probe, which publishes the other half of what the
// metadata server's placement needs.

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/kube"
)

// AdvertiseSubnets reads this node's interfaces and sets nodeName's subnet
// labels to match: one per IPv4 subnet, valued with the interface on it, and
// none for a subnet the node no longer has. Runs once at process start, like
// the KVM probe.
func AdvertiseSubnets(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) error {
	return advertiseSubnets(ctx, kubeClient, nodeName, func() ([]inventory.Interface, error) {
		return inventory.ReadInterfaces(inventory.Config{})
	})
}

func advertiseSubnets(
	ctx context.Context, kubeClient kubernetes.Interface, nodeName string,
	read func() ([]inventory.Interface, error),
) error {
	ifaces, err := read()
	if err != nil {
		return fmt.Errorf("read the interfaces of node %s: %w", nodeName, err)
	}
	want := subnetsOf(ifaces)

	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}
	have := subnetLabels(node.Labels)
	if maps.Equal(have, want) {
		klog.Infof("subnet probe: node %s labels already match %v", nodeName, want)
		return nil
	}

	// A merge patch removes a label given as null.
	labels := make(map[string]any, len(have)+len(want))
	for key := range have {
		labels[key] = nil
	}
	for key, iface := range want {
		labels[key] = iface
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return fmt.Errorf("build the subnet label patch: %w", err)
	}
	if _, err := kubeClient.CoreV1().Nodes().Patch(
		ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patch the subnet labels of node %s: %w", nodeName, err)
	}
	klog.Infof("subnet probe: node %s labeled %v", nodeName, want)
	return nil
}

// subnetsOf maps each IPv4 subnet the interfaces are attached to onto the
// interface attached to it. Loopback and link-local addresses are left out:
// every node has them and they reach no other node. Where two interfaces share
// a subnet, the first by name is kept, so every run publishes the same one.
func subnetsOf(ifaces []inventory.Interface) map[string]string {
	sorted := slices.SortedFunc(slices.Values(ifaces), func(a, b inventory.Interface) int {
		return strings.Compare(a.Name, b.Name)
	})
	subnets := make(map[string]string)
	for _, iface := range sorted {
		if iface.Loopback {
			continue
		}
		for _, cidr := range iface.Addresses {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsLoopback() || prefix.Addr().IsLinkLocalUnicast() {
				continue
			}
			key := kube.SubnetLabel(prefix)
			if _, taken := subnets[key]; !taken {
				subnets[key] = iface.Name
			}
		}
	}
	return subnets
}

// subnetLabels are the labels under kube.LabelSubnetPrefix.
func subnetLabels(labels map[string]string) map[string]string {
	subnets := make(map[string]string)
	for key, value := range labels {
		if strings.HasPrefix(key, kube.LabelSubnetPrefix) {
			subnets[key] = value
		}
	}
	return subnets
}
