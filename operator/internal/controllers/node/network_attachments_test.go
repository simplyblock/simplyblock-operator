// The attachment list a storage node's SPDK pod is started with: one element per
// network attached through a definition, named so that no interface name
// depends on the order Multus assigns.

package node

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	attachNamespace = "simplyblock"
	firstData       = "data0"
	// nicAddress is the address the NIC a definition moves holds on the host.
	nicAddress = "10.10.10.101/24"
)

func viaDefinition(name string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		Name: name,
		Attachment: simplyblockv1alpha2.NetworkAttachment{
			NetworkAttachmentRef: &simplyblockv1alpha2.NetworkDefinitionReference{Name: name + "-def"},
		},
	}
}

func viaHost(name, iface string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		Name:       name,
		Attachment: simplyblockv1alpha2.NetworkAttachment{HostInterface: iface},
	}
}

// statedAddresses is the addresses the operator assigned a node, by network name.
func statedAddresses(pairs ...string) map[string]string {
	assigned := map[string]string{}
	for i := 0; i < len(pairs); i += 2 {
		assigned[pairs[i]] = pairs[i+1]
	}
	return assigned
}

func build(
	t *testing.T, network *simplyblockv1alpha2.StorageNetworkSpec, assigned map[string]string,
) []Attachment {
	t.Helper()
	got, err := BuildAttachments(attachNamespace, network, assigned)
	if err != nil {
		t.Fatalf("BuildAttachments: %v", err)
	}
	return got
}

// U-01
func TestAttachmentsAreNamedInTheOrderTheClusterListsThem(t *testing.T) {
	network := &simplyblockv1alpha2.StorageNetworkSpec{
		Management: &[]simplyblockv1alpha2.StorageNetwork{viaDefinition("management")}[0],
		Data:       []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a"), viaDefinition("storage-b")},
	}
	config := statedAddresses("management", "10.0.0.5/24", "storage-a", nicAddress, "storage-b", "10.20.20.101/24")

	got := build(t, network, config)

	want := []struct{ name, iface, ip string }{
		{"management-def", "mgmt0", "10.0.0.5/24"},
		{"storage-a-def", firstData, nicAddress},
		{"storage-b-def", "data1", "10.20.20.101/24"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d attachments, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Interface != w.iface || len(got[i].IPs) != 1 || got[i].IPs[0] != w.ip {
			t.Errorf("attachment %d = %+v, want %+v", i, got[i], w)
		}
		if wantNetwork := []string{"management", "storage-a", "storage-b"}[i]; got[i].Network != wantNetwork {
			t.Errorf("attachment %d network = %q, want %q", i, got[i].Network, wantNetwork)
		}
		if got[i].Namespace != attachNamespace {
			t.Errorf("attachment %d namespace = %q", i, got[i].Namespace)
		}
	}
}

// U-02
func TestAnAttachmentCarriesTheAddressAndNeverARoute(t *testing.T) {
	network := &simplyblockv1alpha2.StorageNetworkSpec{Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a")}}
	got := build(t, network, statedAddresses("storage-a", nicAddress))

	encoded, err := AnnotationValue(got)
	if err != nil {
		t.Fatalf("AnnotationValue: %v", err)
	}
	if strings.Contains(encoded, "default-route") || strings.Contains(encoded, "gateway") {
		t.Fatalf("the annotation sets a route: %s", encoded)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("the annotation is not a JSON list: %v", err)
	}
	if len(decoded) != 1 || decoded[0]["name"] != "storage-a-def" || decoded[0]["interface"] != firstData ||
		decoded[0]["namespace"] != attachNamespace {
		t.Fatalf("annotation = %s", encoded)
	}
	if ips, _ := decoded[0]["ips"].([]any); len(ips) != 1 || ips[0] != nicAddress {
		t.Fatalf("ips = %v", decoded[0]["ips"])
	}
}

// U-03
func TestWithoutAManagementNetworkTheDataNetworksStartAtDataZero(t *testing.T) {
	network := &simplyblockv1alpha2.StorageNetworkSpec{Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a")}}
	got := build(t, network, statedAddresses("storage-a", nicAddress))

	if len(got) != 1 || got[0].Interface != firstData {
		t.Fatalf("got %+v", got)
	}
}

// U-04
func TestAHostInterfaceNetworkProducesNoAttachment(t *testing.T) {
	network := &simplyblockv1alpha2.StorageNetworkSpec{
		Management: &[]simplyblockv1alpha2.StorageNetwork{viaHost("management", "eth0")}[0],
		Data:       []simplyblockv1alpha2.StorageNetwork{viaHost("storage-a", "eth1")},
	}
	if got := build(t, network, nil); len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
	if got := build(t, nil, nil); len(got) != 0 {
		t.Fatalf("a cluster with no network group produced %+v", got)
	}
}

// U-05
func TestAMissingAddressIsAnErrorNamingTheNetwork(t *testing.T) {
	network := &simplyblockv1alpha2.StorageNetworkSpec{
		Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a"), viaDefinition("storage-b")},
	}
	_, err := BuildAttachments(attachNamespace, network, statedAddresses("storage-a", nicAddress))
	if err == nil || !strings.Contains(err.Error(), "storage-b") {
		t.Fatalf("err = %v, want one naming storage-b", err)
	}
}

// U-06
func TestAtMostEightDataNetworksAreBuilt(t *testing.T) {
	for _, tc := range []struct {
		count   int
		wantErr bool
	}{{8, false}, {9, true}} {
		t.Run(fmt.Sprintf("%d", tc.count), func(t *testing.T) {
			network := &simplyblockv1alpha2.StorageNetworkSpec{}
			var pairs []string
			for i := 0; i < tc.count; i++ {
				name := fmt.Sprintf("net-%d", i)
				network.Data = append(network.Data, viaDefinition(name))
				pairs = append(pairs, name, fmt.Sprintf("10.%d.0.1/24", i))
			}
			got, err := BuildAttachments(attachNamespace, network, statedAddresses(pairs...))
			if tc.wantErr {
				if err == nil {
					t.Fatal("a ninth data network was built")
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildAttachments: %v", err)
			}
			if len(got) != 8 || got[0].Interface != firstData || got[7].Interface != "data7" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func viaPodNetwork(name string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		Name:       name,
		Attachment: simplyblockv1alpha2.NetworkAttachment{PodNetwork: &simplyblockv1alpha2.PodNetworkAttachment{}},
	}
}

// A pod on the pod network attaches to nothing: no element, no annotation.
func TestAPodNetworkHasNoAttachment(t *testing.T) {
	got := build(t, &simplyblockv1alpha2.StorageNetworkSpec{
		Data: []simplyblockv1alpha2.StorageNetwork{viaPodNetwork("cni")},
	}, nil)

	if len(got) != 0 {
		t.Fatalf("attachments = %+v, want none", got)
	}
}
