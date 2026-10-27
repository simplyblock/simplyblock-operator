// Validation of the CEL rules compiled into the StorageCluster CRD for
// spec.storageNodes.network, run against a real apiserver like the rest of this
// package's CEL suite. The rules decide how a cluster may state its storage
// networks: one way only, all through definitions or all as host interfaces,
// set at creation only, and never a definition on an RDMA fabric.

package cluster

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

func viaDefinition(name, definition string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		Name: name,
		Attachment: simplyblockv1alpha2.NetworkAttachment{
			NetworkAttachmentRef: &simplyblockv1alpha2.NetworkDefinitionReference{Name: definition},
		},
	}
}

func aPool(subnet, first, last string) *simplyblockv1alpha2.NetworkAddressPool {
	return &simplyblockv1alpha2.NetworkAddressPool{Subnet: subnet, First: first, Last: last}
}

func viaHostInterface(name, iface string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		Name:       name,
		Attachment: simplyblockv1alpha2.NetworkAttachment{HostInterface: iface},
	}
}

func clusterWithNetwork(
	network *simplyblockv1alpha2.StorageNetworkSpec,
) *simplyblockv1alpha2.StorageCluster {
	return &simplyblockv1alpha2.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "net-", Namespace: "default"},
		Spec: simplyblockv1alpha2.StorageClusterSpec{
			MaxSubsystemCount: ptr.To(int32(10)),
			VCPUCount:         ptr.To(int32(6)),
			StorageNodes:      &simplyblockv1alpha2.StorageNodesSpec{Network: network},
		},
	}
}

// create applies the cluster and reports the apiserver's answer. A cluster that
// was accepted is removed when the test ends.
func create(t *testing.T, apiClient client.Client, cluster *simplyblockv1alpha2.StorageCluster) error {
	t.Helper()
	err := apiClient.Create(context.Background(), cluster)
	if err == nil {
		t.Cleanup(func() { _ = apiClient.Delete(context.Background(), cluster) })
	}
	return err
}

func wantAccepted(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected the apiserver to accept the cluster, got: %v", err)
	}
}

func wantRejected(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the apiserver to reject the cluster, but it was accepted")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("rejected for the wrong reason, want %q in: %v", reason, err)
	}
}

// I-56: a pool is optional on a network attached through a definition, and a
// network on a host interface carries the host's own addresses and has none.
func TestStorageNetworkPoolIsOptionalAndNotForHostInterfaces(t *testing.T) {
	apiClient := apiServer(t)

	t.Run("a definition network without a pool", func(t *testing.T) {
		wantAccepted(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a", "storage-data")},
		})))
	})
	t.Run("a host interface network with a pool", func(t *testing.T) {
		network := viaHostInterface("storage-a", "eth1")
		network.AddressPool = aPool("10.10.10.0/24", "10.10.10.101", "10.10.10.110")
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{network},
		})), "addressPool is not allowed with hostInterface")
	})
}

// I-12, I-47: a pool is a range inside its subnet, written as IPv4 addresses.
func TestStorageNetworkPoolIsARangeInsideItsSubnet(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		name                string
		subnet, first, last string
		reason              string
	}{
		{"a range inside the subnet", "10.10.10.0/24", "10.10.10.101", "10.10.10.110", ""},
		{"one address", "10.10.10.0/24", "10.10.10.101", "10.10.10.101", ""},
		{"first outside the subnet", "10.10.10.0/24", "10.10.11.5", "10.10.10.110", "first and last must lie inside subnet"},
		{"last outside the subnet", "10.10.10.0/24", "10.10.10.5", "10.10.11.110", "first and last must lie inside subnet"},
		{"a subnet without a prefix length", "10.10.10.0", "10.10.10.5", "10.10.10.9", "should match"},
		{"an IPv6 subnet", "fd00::/64", "10.10.10.5", "10.10.10.9", "should match"},
		{"a first that is not an address", "10.10.10.0/24", "ten", "10.10.10.9", "should match"},
		{"an out-of-range octet", "10.10.10.0/24", "10.10.10.256", "10.10.10.9", "should match"},
		{"a subnet alone", "10.10.10.0/24", "", "", ""},
		{"a first without a last", "10.10.10.0/24", "10.10.10.5", "", "first and last are set together"},
		{"a last without a first", "10.10.10.0/24", "", "10.10.10.9", "first and last are set together"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := viaDefinition("storage-a", "storage-data")
			network.AddressPool = aPool(tc.subnet, tc.first, tc.last)
			err := create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
				Data: []simplyblockv1alpha2.StorageNetwork{network},
			}))
			if tc.reason == "" {
				wantAccepted(t, err)
				return
			}
			wantRejected(t, err, tc.reason)
		})
	}
}

// I-02: the legacy interface names and the network group are two ways of stating
// one thing, so a cluster cannot state both.
func TestStorageClusterNetworkExcludesTheLegacyInterfaces(t *testing.T) {
	apiClient := apiServer(t)
	network := &simplyblockv1alpha2.StorageNetworkSpec{
		Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a", "storage-data")},
	}

	for name, mutate := range map[string]func(*simplyblockv1alpha2.StorageNodesSpec){
		"mgmtInterface":  func(s *simplyblockv1alpha2.StorageNodesSpec) { s.MgmtInterface = "eth0" },
		"dataInterfaces": func(s *simplyblockv1alpha2.StorageNodesSpec) { s.DataInterfaces = []string{"eth1"} },
	} {
		t.Run(name, func(t *testing.T) {
			cluster := clusterWithNetwork(network)
			mutate(cluster.Spec.StorageNodes)
			wantRejected(t, create(t, apiClient, cluster),
				"network cannot be combined with mgmtInterface or dataInterfaces")
		})
	}
}

// I-03: an attachment is a definition or a host interface, and nothing else.
func TestStorageNetworkAttachmentHoldsExactlyOneCase(t *testing.T) {
	apiClient := apiServer(t)

	both := viaDefinition("storage-a", "storage-data")
	both.Attachment.HostInterface = "eth1"
	neither := simplyblockv1alpha2.StorageNetwork{Name: "storage-a"}

	for name, network := range map[string]simplyblockv1alpha2.StorageNetwork{"both": both, "neither": neither} {
		t.Run(name, func(t *testing.T) {
			wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
				Data: []simplyblockv1alpha2.StorageNetwork{network},
			})), "exactly one of networkAttachmentRef, hostInterface and podNetwork must be set")
		})
	}
}

// I-04: a pod is on the host network or it is not, so the networks of a cluster
// are all definitions or all host interfaces.
func TestStorageClusterNetworksShareOneAttachmentKind(t *testing.T) {
	apiClient := apiServer(t)
	const mixed = "all networks must use networkAttachmentRef, or all must use hostInterface"

	t.Run("management by definition and data by host interface", func(t *testing.T) {
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Management: ptr.To(viaDefinition("management", "storage-mgmt")),
			Data:       []simplyblockv1alpha2.StorageNetwork{viaHostInterface("storage-a", "eth1")},
		})), mixed)
	})
	t.Run("two data networks of different kinds", func(t *testing.T) {
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{
				viaDefinition("storage-a", "storage-data"), viaHostInterface("storage-b", "eth2"),
			},
		})), mixed)
	})
	t.Run("all host interfaces", func(t *testing.T) {
		wantAccepted(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Management: ptr.To(viaHostInterface("management", "eth0")),
			Data:       []simplyblockv1alpha2.StorageNetwork{viaHostInterface("storage-a", "eth1")},
		})))
	})
}

// I-05: the control plane resolves the networks at node-add and never reads the
// field again, and a cluster that already runs on the host network must stay on
// it through an upgrade. So the field is present from creation or never: it cannot
// be added to a cluster later, and it cannot change or go away.
func TestStorageClusterNetworkIsSetAtCreationOnly(t *testing.T) {
	apiClient := apiServer(t)
	ctx := context.Background()
	const frozen = "set when the cluster is created"

	network := func() *simplyblockv1alpha2.StorageNetworkSpec {
		return &simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a", "storage-data")},
		}
	}

	t.Run("a cluster created without it cannot gain it", func(t *testing.T) {
		cluster := clusterWithNetwork(nil)
		wantAccepted(t, create(t, apiClient, cluster))

		cluster.Spec.StorageNodes.Network = network()
		err := apiClient.Update(ctx, cluster)
		if err == nil || !strings.Contains(err.Error(), frozen) {
			t.Fatalf("adding network to an existing cluster: err = %v, want a refusal saying it is %q", err, frozen)
		}
	})

	t.Run("a cluster created without it is otherwise updatable", func(t *testing.T) {
		cluster := clusterWithNetwork(nil)
		wantAccepted(t, create(t, apiClient, cluster))

		cluster.Spec.StorageNodes.MgmtInterface = "eth0"
		if err := apiClient.Update(ctx, cluster); err != nil {
			t.Fatalf("an unrelated update of a host-network cluster was refused: %v", err)
		}
	})

	for name, mutate := range map[string]func(*simplyblockv1alpha2.StorageCluster){
		"changing a definition": func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.StorageNodes.Network.Data[0].Attachment.NetworkAttachmentRef.Name = "other"
		},
		"adding a network": func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.StorageNodes.Network.Data = append(c.Spec.StorageNodes.Network.Data,
				viaDefinition("storage-b", "storage-data-b"))
		},
		"clearing the group": func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.StorageNodes.Network = nil
		},
	} {
		t.Run("a cluster created with it, "+name, func(t *testing.T) {
			cluster := clusterWithNetwork(network())
			wantAccepted(t, create(t, apiClient, cluster))

			var stored simplyblockv1alpha2.StorageCluster
			if err := apiClient.Get(ctx, client.ObjectKeyFromObject(cluster), &stored); err != nil {
				t.Fatalf("reading the cluster back: %v", err)
			}
			mutate(&stored)
			err := apiClient.Update(ctx, &stored)
			if err == nil {
				t.Fatal("expected the apiserver to reject the update, but it was accepted")
			}
			if !strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), frozen) {
				t.Fatalf("rejected for the wrong reason: %v", err)
			}
		})
	}
}

// I-06: the attachment plugins this design targets cannot carry RDMA, so a
// fabric that includes it states its data networks as host interfaces.
func TestStorageClusterRDMAFabricRefusesDefinitionNetworks(t *testing.T) {
	apiClient := apiServer(t)
	const refused = "a data network attached through a NetworkAttachmentDefinition serves NVMe/TCP only"

	byDefinition := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
		Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a", "storage-data")},
	})
	byDefinition.Spec.FabricType = "rdma"
	wantRejected(t, create(t, apiClient, byDefinition), refused)

	byHostInterface := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
		Data: []simplyblockv1alpha2.StorageNetwork{viaHostInterface("storage-a", "eth1")},
	})
	byHostInterface.Spec.FabricType = "rdma"
	wantAccepted(t, create(t, apiClient, byHostInterface))

	tcp := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
		Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-a", "storage-data")},
	})
	tcp.Spec.FabricType = "tcp"
	wantAccepted(t, create(t, apiClient, tcp))
}

// I-07: eight data networks is the ceiling.
func TestStorageClusterDataNetworksAreBoundedAtEight(t *testing.T) {
	apiClient := apiServer(t)

	for _, tc := range []struct {
		count    int
		accepted bool
	}{{1, true}, {8, true}, {9, false}} {
		t.Run(fmt.Sprintf("%d networks", tc.count), func(t *testing.T) {
			var data []simplyblockv1alpha2.StorageNetwork
			for i := 0; i < tc.count; i++ {
				data = append(data, viaDefinition(fmt.Sprintf("storage-%d", i), "storage-data"))
			}
			err := create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{Data: data}))
			if tc.accepted {
				wantAccepted(t, err)
				return
			}
			wantRejected(t, err, "Too many")
		})
	}
}

// I-08: a network's name is the key the nodes' addresses and status use, so it
// is unique within the cluster and a DNS label.
func TestStorageNetworkNamesAreUniqueDNSLabels(t *testing.T) {
	apiClient := apiServer(t)

	t.Run("duplicate", func(t *testing.T) {
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{
				viaDefinition("storage-a", "storage-data"), viaDefinition("storage-a", "storage-data-b"),
			},
		})), "Duplicate value")
	})
	for _, name := range []string{"Storage_A", "-leading", "trailing-", strings.Repeat("a", 64)} {
		t.Run("invalid "+name, func(t *testing.T) {
			err := create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
				Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition(name, "storage-data")},
			}))
			if err == nil {
				t.Fatal("expected the apiserver to reject the name, but it was accepted")
			}
		})
	}
	t.Run("a label of 63 characters", func(t *testing.T) {
		wantAccepted(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition(strings.Repeat("a", 63), "storage-data")},
		})))
	})
}

func viaPodNetwork(name string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		Name:       name,
		Attachment: simplyblockv1alpha2.NetworkAttachment{PodNetwork: &simplyblockv1alpha2.PodNetworkAttachment{}},
	}
}

// A pod network is the whole declaration: the pod has one interface, which
// carries the RPC too, and its address comes from the cluster CNI, not a pool.
func TestStorageClusterPodNetworkIsTheWholeDeclaration(t *testing.T) {
	apiClient := apiServer(t)
	const alone = "a podNetwork attachment is the only data network and has no management network"

	t.Run("alone", func(t *testing.T) {
		wantAccepted(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{viaPodNetwork("cni")},
		})))
	})
	t.Run("with a pool", func(t *testing.T) {
		pooled := viaPodNetwork("cni")
		pooled.AddressPool = aPool("10.10.10.0/24", "10.10.10.101", "10.10.10.110")
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{pooled},
		})), "addressPool is not allowed with podNetwork")
	})
	t.Run("with a management network", func(t *testing.T) {
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Management: ptr.To(viaHostInterface("management", "eth0")),
			Data:       []simplyblockv1alpha2.StorageNetwork{viaPodNetwork("cni")},
		})), alone)
	})
	t.Run("as the management network", func(t *testing.T) {
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Management: ptr.To(viaPodNetwork("cni")),
		})), alone)
	})
	t.Run("beside another data network", func(t *testing.T) {
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			Data: []simplyblockv1alpha2.StorageNetwork{viaPodNetwork("cni"), viaHostInterface("storage-b", "eth2")},
		})), alone)
	})
}
