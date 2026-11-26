// Which of a multi-socket worker's backend nodes fills which slot.
//
// A slot is a position in the cluster's socketsToUse list, times nodesPerSocket,
// plus the node's position on that socket. The control plane reports the socket
// each node runs on, so the match reads the socket rather than inferring it from
// where the node's RPC port falls among the worker's other nodes.

package node

import (
	"context"
	"testing"

	"k8s.io/utils/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// onSocket is a reading of worker-1's node on one socket.
func onSocket(uuid string, socket int32, rpcPort int32) NodeReading {
	return NodeReading{
		UUID:         uuid,
		Status:       nodeStatusOnline,
		ManagementIP: workerStorageIP,
		SystemUUID:   workerSystemUUID,
		RPCPort:      rpcPort,
		Socket:       ptr.To(socket),
	}
}

// matchAtSlot runs the match for one slot of a cluster whose workers use the
// given sockets.
func matchAtSlot(
	t *testing.T, readings []NodeReading, sockets []string, perSocket int32, slot int32,
) (NodeReading, bool) {
	t.Helper()
	r, node, cluster := aMatcher(t, readings)
	cluster.Spec.StorageNodes = &simplyblockv1alpha2.StorageNodesSpec{SocketsToUse: sockets}
	if perSocket > 0 {
		cluster.Spec.StorageNodes.NodesPerSocket = ptr.To(perSocket)
	}
	node.Spec.Slot = ptr.To(slot)

	reading, found, err := r.matchBackendNode(context.Background(), node, cluster)
	if err != nil {
		t.Fatalf("matchBackendNode: %v", err)
	}
	return reading, found
}

// A slot whose socket has no node yet is not found, even though another socket's
// node sits where the slot falls in the port-sorted list.
//
// Regression: simplyblock-operator#408 — with socket 0's node missing (an add that
// failed part way, or a node removed) the worker's only node was socket 1's, and
// slot 0 took it, so the operator managed socket 1's node under socket 0's object.
func TestASlotWhoseSocketHasNoNodeIsNotGivenAnotherSocketsNode(t *testing.T) {
	_, found := matchAtSlot(t,
		[]NodeReading{onSocket("socket-1-node", 1, 4423)},
		[]string{"0", "1"}, 0, 0)
	if found {
		t.Error("slot 0 (socket 0) was matched to socket 1's node")
	}
}

// A node re-added on a lower port than its neighbor still fills its own slot.
//
// Regression: simplyblock-operator#408 — ports are allocated first-fit, so a node
// removed and added again can take a port below the other socket's node, and
// the port-sorted list then swapped the two slots.
func TestANodeReaddedOnALowerPortKeepsItsSlot(t *testing.T) {
	readings := []NodeReading{
		onSocket("socket-0-node", 0, 4430),
		onSocket("socket-1-node", 1, 4422),
	}
	for slot, want := range []string{"socket-0-node", "socket-1-node"} {
		reading, found := matchAtSlot(t, readings, []string{"0", "1"}, 0, int32(slot))
		if !found || reading.UUID != want {
			t.Errorf("slot %d matched %q (found=%v), want %q", slot, reading.UUID, found, want)
		}
	}
}

// A socket list in descending order puts the first-listed socket in slot 0,
// which is the order the storage node configured them in.
func TestASocketListOutOfOrderMatchesByPosition(t *testing.T) {
	readings := []NodeReading{
		onSocket("socket-1-node", 1, 4422),
		onSocket("socket-0-node", 0, 4423),
	}
	for slot, want := range []string{"socket-1-node", "socket-0-node"} {
		reading, found := matchAtSlot(t, readings, []string{"1", "0"}, 0, int32(slot))
		if !found || reading.UUID != want {
			t.Errorf("slot %d matched %q (found=%v), want %q", slot, reading.UUID, found, want)
		}
	}
}

// With several nodes per socket, a slot takes the node at its position on its own
// socket, whatever is missing on the other sockets.
func TestASlotCountsOnlyTheNodesOfItsOwnSocket(t *testing.T) {
	readings := []NodeReading{
		// Socket 0's second node is missing.
		onSocket("s0-a", 0, 4422),
		onSocket("s1-a", 1, 4424),
		onSocket("s1-b", 1, 4425),
	}
	cases := []struct {
		slot  int32
		want  string
		found bool
	}{
		{slot: 0, want: "s0-a", found: true},
		{slot: 1, found: false},
		{slot: 2, want: "s1-a", found: true},
		{slot: 3, want: "s1-b", found: true},
	}
	for _, c := range cases {
		reading, found := matchAtSlot(t, readings, []string{"0", "1"}, 2, c.slot)
		if found != c.found || (found && reading.UUID != c.want) {
			t.Errorf("slot %d matched %q (found=%v), want %q (found=%v)",
				c.slot, reading.UUID, found, c.want, c.found)
		}
	}
}

// A control plane that does not report the socket is matched by port order, the
// only identity it gives.
func TestAControlPlaneWithoutTheSocketIsMatchedByPortOrder(t *testing.T) {
	older := func(uuid string, port int32) NodeReading {
		reading := onSocket(uuid, 0, port)
		reading.Socket = nil
		return reading
	}
	readings := []NodeReading{older("second", 4423), older("first", 4422)}
	for slot, want := range []string{"first", "second"} {
		reading, found := matchAtSlot(t, readings, []string{"0", "1"}, 0, int32(slot))
		if !found || reading.UUID != want {
			t.Errorf("slot %d matched %q (found=%v), want %q", slot, reading.UUID, found, want)
		}
	}
}

// A cluster that lists no sockets runs on socket 0 alone, which is the storage
// node's default.
func TestNoSocketListMeansSocketZero(t *testing.T) {
	reading, found := matchAtSlot(t,
		[]NodeReading{onSocket("only", 0, 4422)}, nil, 0, 0)
	if !found || reading.UUID != "only" {
		t.Errorf("matched %q (found=%v), want only", reading.UUID, found)
	}
}
