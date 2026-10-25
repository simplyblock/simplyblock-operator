// The choice of a node's address at CheckingNetwork: the first free address of
// the network's pool is recorded in status before anything is attached, kept for
// the life of the node, and shared by no two nodes of a cluster.

package node

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

const (
	poolNetwork = "storage-a"
	firstInPool = "10.10.10.101/24"
	nextInPool  = "10.10.10.102/24"
)

func assignedOn(t *testing.T, r *StorageNodeReconciler) string {
	t.Helper()
	node, _ := nodeAndClusterOf(t, r)
	for _, n := range node.Status.Network {
		if n.Network == poolNetwork {
			return n.Assigned
		}
	}
	return ""
}

func TestTheFirstPassChoosesTheFirstAddressOfThePoolAndProbesIt(t *testing.T) {
	r, apiClient := aProbeWorld(t)

	if _, done, err := check(t, r); err != nil || done {
		t.Fatalf("done=%v err=%v, want the probe created and the node waiting", done, err)
	}

	if got := assignedOn(t, r); got != firstInPool {
		t.Fatalf("assigned = %q, want the first address of the pool", got)
	}
	pod, found := probeOf(t, apiClient)
	if !found || !envHas(pod.Spec.Containers[0].Env, "ARPING", arpingBatch(101, 110)) {
		t.Fatalf("the probe was not asked to check the assigned address and the candidates after it: %+v", pod)
	}
}

// arpingBatch is the ARPING value of a probe that checks the addresses from
// 10.10.10.<from> to 10.10.10.<to> on the first data interface.
func arpingBatch(from, to int) string {
	var pairs []string
	for i := from; i <= to; i++ {
		pairs = append(pairs, fmt.Sprintf("data0=10.10.10.%d", i))
	}
	return strings.Join(pairs, ",")
}

// answering is the guest probe's termination message when the addresses of the
// given last octets answer the duplicate-address check and the rest of the batch
// from 101 to 110 does not.
func answering(octets ...int) string {
	var duplicates []string
	for i := 101; i <= 110; i++ {
		replies := 0
		if slices.Contains(octets, i) {
			replies = 1
		}
		duplicates = append(duplicates, fmt.Sprintf(
			`{"interface":"data0","address":"10.10.10.%d","replies":%d}`, i, replies))
	}
	return `{"interfaces":[{"name":"data0","mtu":1500}],"duplicates":[` + strings.Join(duplicates, ",") + `],"peers":[]}`
}

func TestAnAddressAnotherNodeHoldsIsNotChosen(t *testing.T) {
	r, _ := aProbeWorld(t, aPeerOnTheNetwork("peer-node", firstInPool))

	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}

	if got := assignedOn(t, r); got != nextInPool {
		t.Fatalf("assigned = %q, want the first address the peer does not hold", got)
	}
}

// The choice is made once. A node that has an address keeps it through every
// later pass, including a pool that has changed under it.
func TestAnAssignedAddressIsKeptAcrossPasses(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	node := nodeRead(t, apiClient)
	node.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
		Network: poolNetwork, Phase: simplyblockv1alpha2.NodeNetworkPhasePending, Assigned: "10.10.10.105/24",
	}}
	if err := apiClient.Status().Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, _, err := check(t, r); err != nil {
			t.Fatal(err)
		}
	}

	if got := assignedOn(t, r); got != "10.10.10.105/24" {
		t.Fatalf("assigned = %q, want the recorded address kept", got)
	}
}

func TestAnExhaustedPoolHoldsTheNodeNamingTheNetwork(t *testing.T) {
	r, _ := aProbeWorld(t)
	for i := range 10 {
		peer := aPeerOnTheNetwork("peer-"+string(rune('a'+i)), fmt.Sprintf("10.10.10.%d/24", 101+i))
		if err := r.Create(t.Context(), peer); err != nil {
			t.Fatal(err)
		}
	}

	_, done, err := check(t, r)

	if done || blockedReason(t, err) != NetworkAddressPoolExhausted {
		t.Fatalf("done=%v err=%v, want the node held for NetworkAddressPoolExhausted", done, err)
	}
}

// Two nodes that chose at the same moment, each unaware of the other, end with
// one address. The node whose name sorts later gives its choice up.
func TestTwoNodesThatChoseTheSameAddressSettleOnTheEarlierName(t *testing.T) {
	for _, tc := range []struct {
		name, peer, want string
	}{
		{"the peer sorts first", "0-peer", nextInPool},
		{"this node sorts first", "z-peer", firstInPool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, apiClient := aProbeWorld(t, aPeerOnTheNetwork(tc.peer, firstInPool))
			node := nodeRead(t, apiClient)
			node.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
				Network: poolNetwork, Phase: simplyblockv1alpha2.NodeNetworkPhasePending, Assigned: firstInPool,
			}}
			if err := apiClient.Status().Update(t.Context(), node); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				if _, _, err := check(t, r); err != nil {
					t.Fatal(err)
				}
			}

			if got := assignedOn(t, r); got != tc.want {
				t.Fatalf("assigned = %q, want %q", got, tc.want)
			}
		})
	}
}

func rejectedOn(t *testing.T, r *StorageNodeReconciler) []string {
	t.Helper()
	node, _ := nodeAndClusterOf(t, r)
	for _, n := range node.Status.Network {
		if n.Network == poolNetwork {
			return n.Rejected
		}
	}
	return nil
}

// A busy start of the subnet costs one probe and not one per address: every
// candidate that answers is turned away in the same pass.
func TestABatchOfOccupiedAddressesIsTurnedAwayInOnePass(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	finishProbe(t, apiClient, attachedAs("10.10.10.101"), answering(101, 102, 103))

	next, done, err := check(t, r)

	if err != nil || done || next != stepCheckingNetwork {
		t.Fatalf("next=%s done=%v err=%v, want the node to stay at the step and not be held", next, done, err)
	}
	want := []string{"10.10.10.101/24", "10.10.10.102/24", "10.10.10.103/24"}
	if got := rejectedOn(t, r); !slices.Equal(got, want) {
		t.Fatalf("rejected = %v, want %v", got, want)
	}
	if got := assignedOn(t, r); got != "" {
		t.Fatalf("assigned = %q, want nothing assigned after the turn-away", got)
	}
	if _, found := probeOf(t, apiClient); found {
		t.Fatal("the probe that found the duplicates was not removed")
	}

	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	if got := assignedOn(t, r); got != "10.10.10.104/24" {
		t.Fatalf("assigned = %q, want the first address not turned away", got)
	}
	pod, _ := probeOf(t, apiClient)
	if !envHas(pod.Spec.Containers[0].Env, "ARPING", arpingBatch(104, 110)) {
		t.Fatalf("the next probe checks %+v, want the batch from .104", pod.Spec.Containers[0].Env)
	}
}

// An address that is free is kept, and the candidates after it that answered are
// remembered, so that a later turn-away or a restart of the step never tries them.
func TestACandidateThatAnswersIsRememberedWhenTheAssignedAddressIsFree(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	finishProbe(t, apiClient, attachedAs("10.10.10.101"), answering(103, 105))

	if _, done, err := check(t, r); err != nil || done {
		t.Fatalf("done=%v err=%v, want the node to go on to the host probe", done, err)
	}

	if got := assignedOn(t, r); got != firstInPool {
		t.Fatalf("assigned = %q, want the free address kept", got)
	}
	if got, want := rejectedOn(t, r), []string{"10.10.10.103/24", "10.10.10.105/24"}; !slices.Equal(got, want) {
		t.Fatalf("rejected = %v, want %v", got, want)
	}
}

// The status field holds 64 addresses. A node that has turned that many away has
// run out of pool in any practical sense, and is held with the reason.
func TestANodeThatTurnedAwayTheMostItKeepsIsHeldNotRetried(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	node := nodeRead(t, apiClient)
	turnedAway := make([]string, 0, maxRejected)
	for i := range maxRejected {
		turnedAway = append(turnedAway, fmt.Sprintf("10.10.11.%d/24", i+1))
	}
	node.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
		Network: poolNetwork, Phase: simplyblockv1alpha2.NodeNetworkPhasePending, Rejected: turnedAway,
	}}
	if err := apiClient.Status().Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}

	_, done, err := check(t, r)

	if done || blockedReason(t, err) != NetworkAddressPoolExhausted {
		t.Fatalf("done=%v err=%v, want the node held for NetworkAddressPoolExhausted", done, err)
	}
}

func TestTurningAwayMoreThanTheNodeKeepsStopsAtTheBound(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	node := nodeRead(t, apiClient)
	turnedAway := make([]string, 0, maxRejected-1)
	for i := range maxRejected - 1 {
		turnedAway = append(turnedAway, fmt.Sprintf("10.10.11.%d/24", i+1))
	}
	node.Status.Network = []simplyblockv1alpha2.NodeNetworkStatus{{
		Network: poolNetwork, Phase: simplyblockv1alpha2.NodeNetworkPhasePending,
		Assigned: firstInPool, Rejected: turnedAway,
	}}
	if err := apiClient.Status().Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	finishProbe(t, apiClient, attachedAs("10.10.10.101"), answering(101, 102, 103))

	if _, _, err := check(t, r); err != nil {
		t.Fatalf("the turn-away itself holds nobody: %v", err)
	}
	if got := len(rejectedOn(t, r)); got != maxRejected {
		t.Fatalf("%d rejected addresses kept, want the bound of %d", got, maxRejected)
	}
	if _, _, err := check(t, r); blockedReason(t, err) != NetworkAddressPoolExhausted {
		t.Fatalf("the next pass err = %v, want the node held for NetworkAddressPoolExhausted", err)
	}
}

// The customer states a subnet and what to stay away from, and the operator takes
// the first address past it.
func TestASubnetWithAnExcludeListIsAllThePoolNeeds(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	cluster := &simplyblockv1alpha2.StorageCluster{}
	if err := apiClient.Get(t.Context(), client.ObjectKey{Namespace: opsNamespace, Name: opsCluster}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.StorageNodes.Network.Data[0].AddressPool = &simplyblockv1alpha2.NetworkAddressPool{
		Subnet: "10.10.10.0/24", Exclude: []string{"10.10.10.1-10.10.10.100"},
	}
	if err := apiClient.Update(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}

	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}

	if got := assignedOn(t, r); got != firstInPool {
		t.Fatalf("assigned = %q, want the first address past the excluded range", got)
	}
}

// Probes of different nodes run at the same time, and each holds the address it
// was given. A candidate that answers because a sibling's probe holds it is not a
// foreign host, so it is not turned away: the owner's own pass settles any real
// clash.
func TestACandidateASiblingHoldsIsNotTurnedAwayWhenTheBatchIsRejected(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(t.Context(), aPeerOnTheNetwork("peer-node", nextInPool)); err != nil {
		t.Fatal(err)
	}
	finishProbe(t, apiClient, attachedAs("10.10.10.101"), answering(101, 102, 103))

	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}

	if got, want := rejectedOn(t, r), []string{"10.10.10.101/24", "10.10.10.103/24"}; !slices.Equal(got, want) {
		t.Fatalf("rejected = %v, want %v, without the address the peer holds", got, want)
	}
}

func TestACandidateASiblingHoldsIsNotRememberedWhenTheAssignedAddressIsFree(t *testing.T) {
	r, apiClient := aProbeWorld(t)
	if _, _, err := check(t, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(t.Context(), aPeerOnTheNetwork("peer-node", "10.10.10.105/24")); err != nil {
		t.Fatal(err)
	}
	finishProbe(t, apiClient, attachedAs("10.10.10.101"), answering(103, 105))

	if _, done, err := check(t, r); err != nil || done {
		t.Fatalf("done=%v err=%v, want the node to go on to the host probe", done, err)
	}

	if got, want := rejectedOn(t, r), []string{"10.10.10.103/24"}; !slices.Equal(got, want) {
		t.Fatalf("rejected = %v, want %v, without the address the peer holds", got, want)
	}
}
