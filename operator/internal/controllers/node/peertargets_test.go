// Where a drain's movable volumes go.
//
// The choice is round-robin over the cluster's online peers, and the two
// properties that matter are spread and stability: concentrating a drained
// node's volumes on whichever peer sorts first refills one node with what
// another was holding, and an assignment that reshuffles between passes means a
// volume whose migration failed comes back against a different target for a
// reason nobody chose.
//
// A drain with no online peer holds rather than fails, because the condition is
// resolved by another node coming back and failing the operation would only mean
// starting it again afterward.
//
// design-storagenode.md §8.2.

package node

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/cpinformer/subscriptions"
)

// movable is the census's movable half, one entry per PersistentVolume.
func movable(names ...string) []managedVolume {
	volumes := make([]managedVolume, 0, len(names))
	for _, name := range names {
		volumes = append(volumes, managedVolume{PVName: name, VolumeUUID: name + "-uuid"})
	}
	return volumes
}

// targetsOf assigns the volumes against the given control plane.
func targetsOf(
	t *testing.T, api *scriptedControlPlane, volumes []managedVolume,
) (map[string]string, error) {
	t.Helper()
	r, _ := anOpsWorld(t, api)
	return r.peerTargets(context.Background(), opsClusterID, opsNodeID, volumes)
}

// Six volumes over two peers is three each, rather than six on the peer that
// happened to sort first.
func TestTheVolumesAreSpreadOverEveryOnlinePeer(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer("node-3333", nodeStatusOnline)

	targets, err := targetsOf(t, api, movable("pv-a", "pv-b", "pv-c", "pv-d", "pv-e", "pv-f"))
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	if len(targets) != 6 {
		t.Fatalf("%d of 6 volumes were assigned a target", len(targets))
	}

	share := map[string]int{}
	for _, target := range targets {
		share[target]++
	}
	for _, peer := range []string{opsPeerID, "node-3333"} {
		if share[peer] != 3 {
			t.Errorf("peer %s took %d of six volumes, want an even three", peer, share[peer])
		}
	}
}

// The node being drained is not somewhere to drain to.
func TestTheDrainedNodeIsNeverItsOwnTarget(t *testing.T) {
	api := aControlPlane().withPeer(opsPeerID, nodeStatusOnline)

	targets, err := targetsOf(t, api, movable("pv-a", "pv-b"))
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	for volume, target := range targets {
		if target == opsNodeID {
			t.Errorf("%s was assigned back to the node being drained", volume)
		}
	}
}

// A peer that is not online cannot take a volume, so it is not offered one.
func TestAnOfflinePeerIsNotATarget(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOffline).
		withPeer("node-3333", nodeStatusOnline)

	targets, err := targetsOf(t, api, movable("pv-a", "pv-b"))
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	for volume, target := range targets {
		if target != "node-3333" {
			t.Errorf("%s was assigned to %s, which is not online", volume, target)
		}
	}
}

// A suspended peer is equally not a target: it accepts no new placement, which
// is the whole of what suspending it did.
func TestASuspendedPeerIsNotATarget(t *testing.T) {
	api := aControlPlane().withPeer(opsPeerID, nodeStatusSuspended)

	_, err := targetsOf(t, api, movable("pv-a"))

	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the drain held for want of a peer", err)
	}
}

// No online peer is a stall rather than a failure: another node coming back
// resolves it, and failing the drain would only mean starting it again.
func TestADrainWithNowhereToMoveToHolds(t *testing.T) {
	_, err := targetsOf(t, aControlPlane(), movable("pv-a"))

	var blocked *blockedStepError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want the drain held rather than failed", err)
	}
	if blocked.reason != NoMigrationTarget {
		t.Errorf("the hold is announced as %q, want %q", blocked.reason, NoMigrationTarget)
	}
}

// The same volumes reach the same peers on every pass, so a migration that
// failed and is recreated is reassigned deliberately by the caller rather than
// by the peer list having reshuffled.
func TestTheAssignmentIsTheSameOnEveryPass(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer("node-3333", nodeStatusOnline).
		withPeer("node-4444", nodeStatusOnline)
	volumes := movable("pv-a", "pv-b", "pv-c", "pv-d")

	first, err := targetsOf(t, api, volumes)
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	second, err := targetsOf(t, api, volumes)
	if err != nil {
		t.Fatalf("choosing targets again: %v", err)
	}

	for volume, target := range first {
		if second[volume] != target {
			t.Errorf("%s was assigned to %s and then to %s", volume, target, second[volume])
		}
	}
}

// Once the stream has delivered the cluster's snapshot, the peers are read from
// it and the control plane is not asked at all.
func TestThePeersComeFromTheStreamOnceItHasDelivered(t *testing.T) {
	api := aControlPlane()
	r, _ := anOpsWorld(t, api)
	r.Nodes = &deliveredNodes{synced: true, nodes: []subscriptions.NodeDTO{
		{ID: opsNodeID, Status: nodeStatusOnline},
		{ID: opsPeerID, Status: nodeStatusOnline},
	}}

	targets, err := r.peerTargets(context.Background(), opsClusterID, opsNodeID, movable("pv-a"))
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	if targets["pv-a"] != opsPeerID {
		t.Errorf("pv-a was assigned to %q, want the peer the stream reported", targets["pv-a"])
	}
	if asked := api.asked("StorageNodes"); asked != 0 {
		t.Errorf("the control plane was asked %d time(s) for what the stream already holds", asked)
	}
}

// A cache that has not delivered its snapshot is not evidence of anything. An
// empty unsynced cache and a cluster with no peers look identical, and reading
// the first as the second would hold a drain that has every peer it needs.
func TestAnUndeliveredStreamFallsBackToTheControlPlane(t *testing.T) {
	api := aControlPlane().withPeer(opsPeerID, nodeStatusOnline)
	r, _ := anOpsWorld(t, api)
	r.Nodes = &deliveredNodes{synced: false}

	targets, err := r.peerTargets(context.Background(), opsClusterID, opsNodeID, movable("pv-a"))
	if err != nil {
		t.Fatalf("choosing targets: %v", err)
	}
	if targets["pv-a"] != opsPeerID {
		t.Errorf("pv-a was assigned to %q, want the peer the control plane reported",
			targets["pv-a"])
	}
}

// deliveredNodes is a storage-node cache, holding whatever the stream is said to
// have delivered and reporting synced only when it has.
type deliveredNodes struct {
	nodes  []subscriptions.NodeDTO
	synced bool
}

func (d *deliveredNodes) Lookup(nodeID string) (cpinformer.Scope, subscriptions.NodeDTO, bool) {
	for _, node := range d.nodes {
		if node.ID == nodeID {
			return cpinformer.Scope{opsClusterID}, node, true
		}
	}
	return nil, subscriptions.NodeDTO{}, false
}

func (d *deliveredNodes) List(cpinformer.Scope) []subscriptions.NodeDTO { return d.nodes }

func (d *deliveredNodes) Synced(cpinformer.Scope) bool { return d.synced }

// Triggers is nil here: these cases drive the reconcile themselves rather than
// waiting to be woken by the stream.
func (d *deliveredNodes) Triggers() <-chan event.GenericEvent { return nil }
