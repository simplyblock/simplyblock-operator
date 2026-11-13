// Validation of the CEL rules compiled into the StorageCluster CRD for
// spec.storageNodes.network, run against a real apiserver like the rest of this
// package's CEL suite. The rules decide how a cluster may state its storage
// networks: exactly one of the pod network, the host network and Multus data
// networks, set at creation only, and never a definition on an RDMA fabric.

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

func viaDefinition(definition string) simplyblockv1alpha2.StorageNetwork {
	return simplyblockv1alpha2.StorageNetwork{
		NetworkAttachmentRef: simplyblockv1alpha2.NetworkDefinitionReference{Name: definition},
	}
}

func aPool(subnet, first, last string) *simplyblockv1alpha2.NetworkAddressPool {
	return &simplyblockv1alpha2.NetworkAddressPool{Subnet: subnet, First: first, Last: last}
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

// I-56: a pool is optional on a data network, and a range inside its subnet.
func TestStorageNetworkPoolIsOptional(t *testing.T) {
	apiClient := apiServer(t)

	t.Run("a definition network without a pool", func(t *testing.T) {
		wantAccepted(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			PodNetwork: true,
			Data:       []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-data")},
		})))
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
			network := viaDefinition("storage-data")
			network.NetworkAttachmentRef.AddressPool = aPool(tc.subnet, tc.first, tc.last)
			err := create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
				PodNetwork: true,
				Data:       []simplyblockv1alpha2.StorageNetwork{network},
			}))
			if tc.reason == "" {
				wantAccepted(t, err)
				return
			}
			wantRejected(t, err, tc.reason)
		})
	}
}

// I-02: dataInterfaces name host NICs, so they are allowed only when the SPDK pods
// run on the host network. The management interface is mgmtInterface whatever the
// mode.
func TestStorageClusterDataInterfacesNeedTheHostNetwork(t *testing.T) {
	apiClient := apiServer(t)
	multus := &simplyblockv1alpha2.StorageNetworkSpec{PodNetwork: true, Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-data")}}
	const refused = "dataInterfaces are allowed only with network.spdkOnHostNetwork"

	for name, network := range map[string]*simplyblockv1alpha2.StorageNetworkSpec{
		"a definition network": multus,
		"the pod network":      {PodNetwork: true},
	} {
		t.Run(name, func(t *testing.T) {
			cluster := clusterWithNetwork(network)
			cluster.Spec.StorageNodes.DataInterfaces = []string{"eth1"}
			wantRejected(t, create(t, apiClient, cluster), refused)
		})
	}

	t.Run("the host network", func(t *testing.T) {
		cluster := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{SpdkOnHostNetwork: true})
		cluster.Spec.StorageNodes.DataInterfaces = []string{"eth1"}
		wantAccepted(t, create(t, apiClient, cluster))
	})

	t.Run("mgmtInterface is accepted beside any network", func(t *testing.T) {
		cluster := clusterWithNetwork(multus)
		cluster.Spec.StorageNodes.MgmtInterface = "eth0"
		wantAccepted(t, create(t, apiClient, cluster))
	})
}

// I-03: the pods are on the host network, which excludes the rest, or on the pod
// network, which data networks may add interfaces to. Data networks need the pod
// network they attach to.
func TestStorageNetworkModes(t *testing.T) {
	apiClient := apiServer(t)
	data := []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-data")}

	for name, tc := range map[string]struct {
		network *simplyblockv1alpha2.StorageNetworkSpec
		reason  string
	}{
		"none of them":                 {&simplyblockv1alpha2.StorageNetworkSpec{}, "one of podNetwork and spdkOnHostNetwork must be set"},
		"false flags and no data list": {&simplyblockv1alpha2.StorageNetworkSpec{PodNetwork: false}, "one of podNetwork and spdkOnHostNetwork must be set"},
		"the pod and host networks":    {&simplyblockv1alpha2.StorageNetworkSpec{PodNetwork: true, SpdkOnHostNetwork: true}, "podNetwork and spdkOnHostNetwork are exclusive"},
		"all three":                    {&simplyblockv1alpha2.StorageNetworkSpec{PodNetwork: true, SpdkOnHostNetwork: true, Data: data}, "podNetwork and spdkOnHostNetwork are exclusive"},
		"data without a pod network":   {&simplyblockv1alpha2.StorageNetworkSpec{Data: data}, "data networks require podNetwork: true"},
		"the host network and data":    {&simplyblockv1alpha2.StorageNetworkSpec{SpdkOnHostNetwork: true, Data: data}, "data networks require podNetwork: true"},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			wantRejected(t, create(t, apiClient, clusterWithNetwork(tc.network)), tc.reason)
		})
	}

	for name, network := range map[string]*simplyblockv1alpha2.StorageNetworkSpec{
		"the pod network":               {PodNetwork: true},
		"the host network":              {SpdkOnHostNetwork: true},
		"the pod network with data":     {PodNetwork: true, Data: data},
		"the pod network with two data": {PodNetwork: true, Data: []simplyblockv1alpha2.StorageNetwork{viaDefinition("a"), viaDefinition("b")}},
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			wantAccepted(t, create(t, apiClient, clusterWithNetwork(network)))
		})
	}
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
			PodNetwork: true,
			Data:       []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-data")},
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
			c.Spec.StorageNodes.Network.Data[0].NetworkAttachmentRef.Name = "other"
		},
		"adding a network": func(c *simplyblockv1alpha2.StorageCluster) {
			c.Spec.StorageNodes.Network.Data = append(c.Spec.StorageNodes.Network.Data,
				viaDefinition("storage-data-b"))
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
// fabric that includes it runs on the host network.
func TestStorageClusterRDMAFabricRefusesDefinitionNetworks(t *testing.T) {
	apiClient := apiServer(t)
	const refused = "a data network attached through a NetworkAttachmentDefinition serves NVMe/TCP only"

	byDefinition := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
		PodNetwork: true,
		Data:       []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-data")},
	})
	byDefinition.Spec.FabricType = "rdma"
	wantRejected(t, create(t, apiClient, byDefinition), refused)

	onTheHost := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{SpdkOnHostNetwork: true})
	onTheHost.Spec.FabricType = "rdma"
	wantAccepted(t, create(t, apiClient, onTheHost))

	tcp := clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
		PodNetwork: true,
		Data:       []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-data")},
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
			for i := range tc.count {
				data = append(data, viaDefinition(fmt.Sprintf("storage-data-%d", i)))
			}
			err := create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{PodNetwork: true, Data: data}))
			if tc.accepted {
				wantAccepted(t, err)
				return
			}
			wantRejected(t, err, "Too many")
		})
	}
}

// I-08: a definition's name is the key the nodes' addresses and status use, so a
// cluster names each definition once.
func TestStorageNetworksNameDistinctDefinitions(t *testing.T) {
	apiClient := apiServer(t)

	t.Run("duplicate", func(t *testing.T) {
		wantRejected(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			PodNetwork: true,
			Data:       []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-data"), viaDefinition("storage-data")},
		})), "data networks must name distinct NetworkAttachmentDefinitions")
	})
	t.Run("two definitions", func(t *testing.T) {
		wantAccepted(t, create(t, apiClient, clusterWithNetwork(&simplyblockv1alpha2.StorageNetworkSpec{
			PodNetwork: true,
			Data:       []simplyblockv1alpha2.StorageNetwork{viaDefinition("storage-data"), viaDefinition("storage-data-b")},
		})))
	})
}
