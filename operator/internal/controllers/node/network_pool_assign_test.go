// A node on a network whose definition is paired with an address pool draws its
// address from the pool and reads no interface. A network without a pool adopts
// the address of its NIC. Two nodes of one pool settle on two addresses.

package node

import (
	"context"
	"fmt"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// viaPool is a network attached through a definition whose addresses come from
// the pool 10.10.10.101 to 10.10.10.103.
func viaPool(name string) *simplyblockv1alpha2.StorageNetworkSpec {
	network := viaDefinition(name)
	pool := poolOf("10.10.10.0/24", "10.10.10.101", "10.10.10.103")
	network.NetworkAttachmentRef.AddressPool = &pool
	return &simplyblockv1alpha2.StorageNetworkSpec{Data: []simplyblockv1alpha2.StorageNetwork{network}}
}

// poolWorld is a node on one pooled macvlan network with nothing recorded yet and
// a host check that records what it is asked.
func poolWorld(t *testing.T, objects ...client.Object) (*StorageNodeReconciler, client.Client, *fakeHost) {
	t.Helper()
	objects = append(objects, aDefinitionConfigured(`{"type":"macvlan","master":"ens4","ipam":{"type":"static"},"capabilities":{"ips":true}}`))
	r, apiClient := onANetwork(t, viaPool(storageNetwork), objects...)
	node := nodeRead(t, apiClient)
	node.Status.Network = nil
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	host := aWorkerWithTheNIC(adoptedAddress)
	r.HostCheck = host
	return r, apiClient, host
}

func aPeerHolding(name, address string) *simplyblockv1alpha2.StorageNode {
	peer := anOpsNode()
	peer.Name = name
	peer.UID = ""
	peer.ResourceVersion = ""
	peer.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
		Network: storageNetwork, Phase: simplyblockv1alpha2.NodeNetworkPhaseAttached, Assigned: address,
	}}
	return peer
}

// I-43
func TestAPooledNetworkTakesTheFirstAddressOfThePoolAndReadsNoInterface(t *testing.T) {
	r, apiClient, host := poolWorld(t)

	if _, done, err := runCheck(t, r); err != nil || !done {
		t.Fatalf("done=%v err=%v, want the node to move on", done, err)
	}

	if got := assignedOn(t, apiClient); got != firstInPool {
		t.Fatalf("assigned = %q, want the first address of the pool", got)
	}
	if len(host.reads) != 0 {
		t.Errorf("the worker was read %d times, want none for a pooled network", len(host.reads))
	}
}

// I-75
func TestAPooledNetworkNeedsNoHostCheck(t *testing.T) {
	r, apiClient, _ := poolWorld(t)
	r.HostCheck = nil

	if _, done, err := runCheck(t, r); err != nil || !done {
		t.Fatalf("done=%v err=%v, want the node to move on without a host check", done, err)
	}
	if got := assignedOn(t, apiClient); got != firstInPool {
		t.Fatalf("assigned = %q, want the first address of the pool", got)
	}
}

// I-44
func TestAnAddressAnotherNodeHoldsIsNotChosen(t *testing.T) {
	r, apiClient, _ := poolWorld(t, aPeerHolding("peer-node", firstInPool))

	if _, _, err := runCheck(t, r); err != nil {
		t.Fatal(err)
	}

	if got := assignedOn(t, apiClient); got != nextInPool {
		t.Fatalf("assigned = %q, want the first address the peer does not hold", got)
	}
}

// I-76: the choice is made once, through a pool that has changed under it.
func TestAPooledAddressIsKeptAcrossPasses(t *testing.T) {
	r, apiClient, _ := poolWorld(t)
	node := nodeRead(t, apiClient)
	node.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
		Network: storageNetwork, Phase: simplyblockv1alpha2.NodeNetworkPhasePending, Assigned: "10.10.10.105/24",
	}}
	if err := apiClient.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, _, err := runCheck(t, r); err != nil {
			t.Fatal(err)
		}
	}

	if got := assignedOn(t, apiClient); got != "10.10.10.105/24" {
		t.Fatalf("assigned = %q, want the recorded address kept", got)
	}
}

// I-45
func TestAnExhaustedPoolHoldsTheNodeNamingTheNetwork(t *testing.T) {
	r, _, _ := poolWorld(t)
	for i := range 3 {
		if err := r.Create(context.Background(), aPeerHolding(fmt.Sprintf("peer-%d", i), fmt.Sprintf("10.10.10.%d/24", 101+i))); err != nil {
			t.Fatal(err)
		}
	}

	_, done, err := runCheck(t, r)

	if done || holdReason(t, err) != NetworkAddressPoolExhausted {
		t.Fatalf("done=%v err=%v, want the node held for %s", done, err, NetworkAddressPoolExhausted)
	}
}

// I-46: two nodes that chose at the same moment end with two addresses, and the
// node whose name sorts later gives its choice up.
func TestTwoNodesThatChoseTheSameAddressSettleOnTheEarlierName(t *testing.T) {
	for _, tc := range []struct {
		name, peer, want string
	}{
		{"the peer sorts first", "0-peer", nextInPool},
		{"this node sorts first", "z-peer", firstInPool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := aPeerHolding(tc.peer, firstInPool)
			peer.Status.Network[0].Phase = simplyblockv1alpha2.NodeNetworkPhasePending
			r, apiClient, _ := poolWorld(t, peer)
			node := nodeRead(t, apiClient)
			node.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
				Network: storageNetwork, Phase: simplyblockv1alpha2.NodeNetworkPhasePending, Assigned: firstInPool,
			}}
			if err := apiClient.Status().Update(context.Background(), node); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				if _, _, err := runCheck(t, r); err != nil {
					t.Fatal(err)
				}
			}

			if got := assignedOn(t, apiClient); got != tc.want {
				t.Fatalf("assigned = %q, want %q", got, tc.want)
			}
		})
	}
}

// I-77: a clash is on the wire, whatever the other cluster calls its network.
func TestAnAddressInThePoolHeldUnderAnotherNetworkNameIsNotChosen(t *testing.T) {
	peer := aPeerHolding("peer-node", firstInPool)
	peer.Status.Network[0].Network = "other-name"
	r, apiClient, _ := poolWorld(t, peer)

	if _, _, err := runCheck(t, r); err != nil {
		t.Fatal(err)
	}

	if got := assignedOn(t, apiClient); got != nextInPool {
		t.Fatalf("assigned = %q, want the address the peer does not hold", got)
	}
}

// I-60: pool presence selects the source, whatever the plugin.
func TestTheAddressSourceFollowsThePoolAndNotThePlugin(t *testing.T) {
	r, apiClient := adoptWorld(t, `{"type":"macvlan","master":"ens4","ipam":{"type":"static"}}`, aWorkerWithTheNIC(adoptedAddress))

	if _, done, err := runCheck(t, r); err != nil || !done {
		t.Fatalf("done=%v err=%v, want the NIC address adopted", done, err)
	}
	if got := assignedOn(t, apiClient); got != adoptedAddress {
		t.Fatalf("assigned = %q, want the NIC's address for a network with no pool", got)
	}
}

// I-78: adoption needs an interface, and a definition that names none cannot
// give one.
func TestAnAdoptedNetworkWhoseDefinitionNamesNoInterfaceHoldsTheNode(t *testing.T) {
	host := aWorkerWithTheNIC(adoptedAddress)
	r, _ := adoptWorld(t, `{"type":"host-device","ipam":{"type":"static"}}`, host)

	_, done, err := runCheck(t, r)

	if done || holdReason(t, err) != NetworkAttachmentFailed {
		t.Fatalf("done=%v err=%v, want the node held for %s", done, err, NetworkAttachmentFailed)
	}
	if len(host.reads) != 0 {
		t.Errorf("the worker was asked %d times for an interface nobody named", len(host.reads))
	}
}
