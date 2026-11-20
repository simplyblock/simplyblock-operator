package node

import (
	"context"
	"errors"
	"maps"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/kube"
)

func interfacesOf(ifaces ...inventory.Interface) func() ([]inventory.Interface, error) {
	return func() ([]inventory.Interface, error) { return ifaces, nil }
}

func nodeLabels(t *testing.T, client *kfake.Clientset) map[string]string {
	t.Helper()
	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return node.Labels
}

// The operator finds the nodes that reach the metadata server's data network by
// these labels, and the macvlan's parent interface by their values.
func TestAdvertiseSubnetsLabelsEachInterfaceSubnet(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	read := interfacesOf(
		inventory.Interface{Name: "br-ex", Addresses: []string{"192.168.10.247/24"}},
		inventory.Interface{Name: "ens16np0", Addresses: []string{"10.10.10.247/24", "fd00::247/64"}},
	)
	if err := advertiseSubnets(context.Background(), client, "node-a", read); err != nil {
		t.Fatalf("advertiseSubnets: %v", err)
	}
	labels := nodeLabels(t, client)
	want := map[string]string{
		kube.LabelSubnetPrefix + "192.168.10.0_24": "br-ex",
		kube.LabelSubnetPrefix + "10.10.10.0_24":   "ens16np0",
	}
	if got := subnetLabels(labels); !maps.Equal(got, want) {
		t.Errorf("subnet labels = %v, want %v", got, want)
	}
}

// A subnet the node no longer has would place the metadata server on a node
// where its data network does not exist. Labels outside the prefix are not the
// probe's and stay.
func TestAdvertiseSubnetsRemovesSubnetsTheNodeLost(t *testing.T) {
	stale := kube.LabelSubnetPrefix + "10.20.0.0_16"
	client := kfake.NewSimpleClientset(testNode(map[string]string{
		stale: "ens17", kube.LabelKVMCapable: vdoCapableTrue,
	}, nil))
	read := interfacesOf(inventory.Interface{Name: "ens16np0", Addresses: []string{"10.10.10.247/24"}})
	if err := advertiseSubnets(context.Background(), client, "node-a", read); err != nil {
		t.Fatalf("advertiseSubnets: %v", err)
	}
	labels := nodeLabels(t, client)
	if _, found := labels[stale]; found {
		t.Errorf("stale label %s still present: %v", stale, labels)
	}
	if labels[kube.LabelKVMCapable] != vdoCapableTrue {
		t.Errorf("label %s was touched: %v", kube.LabelKVMCapable, labels)
	}
}

// Loopback and link-local addresses are on every node and reach no other one.
func TestAdvertiseSubnetsSkipsLoopbackAndLinkLocal(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	read := interfacesOf(
		inventory.Interface{Name: "lo", Loopback: true, Addresses: []string{"127.0.0.1/8"}},
		inventory.Interface{Name: "br-ex", Addresses: []string{"169.254.0.2/17"}},
	)
	if err := advertiseSubnets(context.Background(), client, "node-a", read); err != nil {
		t.Fatalf("advertiseSubnets: %v", err)
	}
	if got := subnetLabels(nodeLabels(t, client)); len(got) != 0 {
		t.Errorf("subnet labels = %v, want none", got)
	}
}

// Two interfaces on one subnet publish one of them, the same one on every run.
func TestAdvertiseSubnetsPicksOneInterfacePerSubnet(t *testing.T) {
	client := kfake.NewSimpleClientset(testNode(nil, nil))
	read := interfacesOf(
		inventory.Interface{Name: "ens17", Addresses: []string{"10.10.10.5/24"}},
		inventory.Interface{Name: "ens16", Addresses: []string{"10.10.10.4/24"}},
	)
	if err := advertiseSubnets(context.Background(), client, "node-a", read); err != nil {
		t.Fatalf("advertiseSubnets: %v", err)
	}
	if got := nodeLabels(t, client)[kube.LabelSubnetPrefix+"10.10.10.0_24"]; got != "ens16" {
		t.Errorf("10.10.10.0/24 published on %q, want ens16, the first by name", got)
	}
}

// A node whose interfaces cannot be read keeps the labels it has.
func TestAdvertiseSubnetsLeavesLabelsWhenInterfacesCannotBeRead(t *testing.T) {
	existing := kube.LabelSubnetPrefix + "10.10.10.0_24"
	client := kfake.NewSimpleClientset(testNode(map[string]string{existing: "ens16np0"}, nil))
	read := func() ([]inventory.Interface, error) { return nil, errors.New("sysfs unreadable") }
	if err := advertiseSubnets(context.Background(), client, "node-a", read); err == nil {
		t.Fatal("advertiseSubnets succeeded on an unreadable interface list")
	}
	if nodeLabels(t, client)[existing] != "ens16np0" {
		t.Errorf("label %s was removed", existing)
	}
}
