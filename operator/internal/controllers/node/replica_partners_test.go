// Where a drain's moves may go, given the nodes that replicate onto each other.
//
// Every node of an HA cluster has its lvstore replicated onto a secondary and,
// at FTT=2, a tertiary. Two facts follow. A volume whose primary is the drained
// node has its replicas on that node's secondary and tertiary, so neither is a
// target. And a move builds the volume on the target's own secondary and
// tertiary too, so a peer that replicates onto the drained node puts the node
// that is leaving on both sides of the copy; such a peer is used only when no
// other is left.
//
// design-storagenode.md §8.2.

package node

import (
	"context"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

const (
	fourthPeerID = "node-4444"
)

// withReplicaPartners sets which nodes a node's lvstore replicates onto.
func (c *scriptedControlPlane) withReplicaPartners(nodeID, secondary, tertiary string) *scriptedControlPlane {
	reading := c.nodes[nodeID]
	reading.SecondaryNodeID, reading.TertiaryNodeID = secondary, tertiary
	c.nodes[nodeID] = reading
	return c
}

// Regression: 2026-10-05-drain-target-is-tertiary — a volume created by
// replication lists only its primary and secondary as its nodes, so the drained
// node's tertiary, which holds the volume's third replica on an FTT=2 cluster,
// was an eligible target, and the control plane refuses a move onto a node
// already holding a replica.
func TestTheDrainedNodesTertiaryIsNeverATarget(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer(otherPeerID, nodeStatusOnline).
		withPeer(fourthPeerID, nodeStatusOnline).
		withReplicaPartners(opsNodeID, opsPeerID, otherPeerID)
	volumes := []managedVolume{{PVName: "pv-1", VolumeUUID: "volume-1",
		ReplicaNodes: []string{opsNodeID, opsPeerID}}}

	targets, err := targetsOf(t, api, volumes)
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	if got := targets["pv-1"]; got != fourthPeerID {
		t.Errorf("the volume is moved to %q, want %s: %s is the drained node's tertiary and holds "+
			"a replica of it", got, fourthPeerID, otherPeerID)
	}
}

// Regression: 2026-10-05-drain-target-replicates-onto-drainee — a peer whose
// secondary or tertiary is the drained node builds the moved volume's replica
// on the node that is leaving, and the drain chose such a peer purely by its
// position in the round-robin.
func TestPeersThatReplicateOntoTheDrainedNodeComeLast(t *testing.T) {
	for name, partners := range map[string][2]string{
		"as its secondary": {opsNodeID, ""},
		"as its tertiary":  {fourthPeerID, opsNodeID},
	} {
		t.Run(name, func(t *testing.T) {
			api := aControlPlane().
				withPeer(opsPeerID, nodeStatusOnline).
				withPeer(otherPeerID, nodeStatusOnline).
				withReplicaPartners(opsPeerID, partners[0], partners[1])

			targets, err := targetsOf(t, api, movable("pv-1"))
			if err != nil {
				t.Fatalf("choosing targets: %v", err)
			}
			if got := targets["pv-1"]; got != otherPeerID {
				t.Errorf("the volume is moved to %q, want %s: %s replicates onto the drained node",
					got, otherPeerID, opsPeerID)
			}
		})
	}
}

// A peer that replicates onto the drained node is still a target when no other
// is left. The control plane skips the departing replica when it builds there,
// and on a small cluster every peer may replicate onto the drained node:
// draining onto one beats holding the drain.
func TestAPeerReplicatingOntoTheDrainedNodeIsUsedWhenNoOtherIsLeft(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withReplicaPartners(opsPeerID, opsNodeID, "")

	targets, err := targetsOf(t, api, movable("pv-1"))
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	if got := targets["pv-1"]; got != opsPeerID {
		t.Errorf("the volume is moved to %q, want the one peer there is", got)
	}
}

// Regression: 2026-10-05-drain-target-is-tertiary — the node stream is what
// target selection reads once it has synced, so it has to carry each node's
// replica partners as well.
func TestTheNodeStreamCarriesEachNodesReplicaPartners(t *testing.T) {
	r, _ := anOpsWorld(t, aControlPlane())
	r.Nodes = &deliveredNodes{synced: true, nodes: []subscriptions.NodeDTO{
		{ID: opsNodeID, Status: nodeStatusOffline, SecondaryNodeID: opsPeerID, TertiaryNodeID: otherPeerID},
		{ID: opsPeerID, Status: nodeStatusOnline},
		{ID: otherPeerID, Status: nodeStatusOnline},
		{ID: fourthPeerID, Status: nodeStatusOnline},
	}}

	targets, err := r.peerTargets(context.Background(), opsClusterID, opsNodeID, movable("pv-1"), nil)
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	if got := targets["pv-1"]; got != fourthPeerID {
		t.Errorf("the volume is moved to %q, want %s, the one peer holding none of its replicas",
			got, fourthPeerID)
	}
}
