// A drain's fan-out, counted in NVMe-oF subsystems rather than in volumes.
//
// The control plane migrates a subsystem as a whole, so the volumes published
// under one subsystem leave the node at the one cutover. A drain that raised one
// move per volume raised one whole-subsystem migration per sibling, each to its
// own target, and every one of them after the first moved the subsystem again.
//
// design-storagenode.md §8.2 and §8.4.

package node

import (
	"testing"

	vmigration "github.com/simplyblock/simplyblock-operator/internal/volumemigration"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

const (
	subsystemA = "nqn.2023-02.io.simplyblock:cluster:lvol:a"
	subsystemB = "nqn.2023-02.io.simplyblock:cluster:lvol:b"
)

// publishedUnder puts a volume in a subsystem.
func publishedUnder(volume webapi.VolumeInfo, nqn string) webapi.VolumeInfo {
	volume.NQN = nqn
	return volume
}

// Regression: 2026-10-05-drain-per-volume-moves — two volumes of one subsystem
// got a move each, and the second migrated the whole subsystem a second time.
func TestTheVolumesOfOneSubsystemAreGivenOneMove(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		holding(
			publishedUnder(onNode("volume-1", "pvc-1"), subsystemA),
			publishedUnder(onNode("volume-2", "pvc-2"), subsystemA),
			publishedUnder(onNode("volume-3", "pvc-3"), subsystemB))
	mover := &scriptedMover{}
	r, _ := aDraining(t, api, mover,
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false),
		aPersistentVolume("pv-2", "volume-2"), aClaim("pv-2", false),
		aPersistentVolume("pv-3", "volume-3"), aClaim("pv-3", false))

	if _, err := performing(t, r, aDrain(), stepMigratingVolumes); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	named := map[string]bool{}
	for _, request := range mover.started {
		named[request.PVName] = true
	}
	if len(mover.started) != 2 || !named["pv-1"] || !named["pv-3"] {
		t.Errorf("moves were raised for %v, want one per subsystem, each named by its first volume: "+
			"pv-1 for the shared subsystem and pv-3 for its own", named)
	}
}

// Regression: 2026-10-05-drain-per-volume-moves — a subsystem's move lands every
// member's data on the target, so the target may hold a replica of none of
// them, not only of the volume the move is named by.
func TestASubsystemsTargetHoldsNoReplicaOfAnyMember(t *testing.T) {
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer("node-3333", nodeStatusOnline).
		withPeer("node-4444", nodeStatusOnline).
		holding(
			publishedUnder(replicatedOn("volume-1", "pvc-1", opsPeerID), subsystemA),
			publishedUnder(replicatedOn("volume-2", "pvc-2", "node-3333"), subsystemA))
	mover := &scriptedMover{}
	r, _ := aDraining(t, api, mover,
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false),
		aPersistentVolume("pv-2", "volume-2"), aClaim("pv-2", false))

	if _, err := performing(t, r, aDrain(), stepMigratingVolumes); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if len(mover.started) != 1 {
		t.Fatalf("%d moves were raised for one subsystem", len(mover.started))
	}
	if target := mover.started[0].TargetNodeUUID; target != "node-4444" {
		t.Errorf("the subsystem is being moved to %q, want node-4444, the one peer holding "+
			"a replica of neither member", target)
	}
}

// Regression: 2026-10-05-drain-per-volume-moves — during a cutover the control
// plane moves the members' records to the target one at a time. A member still
// reported on the node while the volume its move is named by has already left
// is covered by that move, and a second move for it would migrate the
// subsystem again.
func TestASubsystemMidCutoverIsNotGivenASecondMove(t *testing.T) {
	moved := publishedUnder(onNode("volume-1", "pvc-1"), subsystemA)
	moved.PrimaryNodeUUID = opsPeerID
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		holding(moved, publishedUnder(onNode("volume-2", "pvc-2"), subsystemA))
	mover := &scriptedMover{moves: []vmigration.Move{{
		Name: migrationName(opsNodeID, "pv-1"), PVName: "pv-1", Phase: vmigration.MoveRunning,
	}}}
	r, _ := aDraining(t, api, mover,
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false),
		aPersistentVolume("pv-2", "volume-2"), aClaim("pv-2", false))

	done, err := performing(t, r, aDrain(), stepMigratingVolumes)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if done {
		t.Error("the step finished while the subsystem's move is still running")
	}
	if len(mover.started) != 0 {
		t.Errorf("%d further moves were raised for a subsystem already being moved: %+v",
			len(mover.started), mover.started)
	}
}

// Regression: 2026-10-05-drain-per-volume-moves — progress is reported in
// volumes, and a finished move of a three-volume subsystem moved three.
func TestAFinishedSubsystemMoveCountsEveryVolumeItCarried(t *testing.T) {
	api := aControlPlane().withPeer(opsPeerID, nodeStatusOnline)
	mover := &scriptedMover{moves: []vmigration.Move{{
		Name: migrationName(opsNodeID, "pv-1"), PVName: "pv-1",
		Phase: vmigration.MoveSucceeded, Members: 3,
	}}}
	r, apiClient := aDraining(t, api, mover)

	if _, err := performing(t, r, aDrain(), stepMigratingVolumes); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	got := operationRead(t, apiClient, "a-drain")
	if got.Status.Drain == nil || got.Status.Drain.VolumesMigrated != 3 {
		t.Errorf("drain = %+v, want the 3 volumes the subsystem's move carried", got.Status.Drain)
	}
}

// Regression: 2026-10-06-drain-replicas-of-moved-siblings — the replica
// exclusion was built from the volumes still on the drained node, so a sibling
// whose primary had already moved, mid-cutover or after a retried move,
// contributed none of its replicas, and the move could be sent onto one of
// them, which the control plane refuses.
func TestASubsystemsTargetAvoidsTheReplicasOfSiblingsOffTheNode(t *testing.T) {
	moved := publishedUnder(replicatedOn("volume-2", "pvc-2", "node-3333"), subsystemA)
	moved.PrimaryNodeUUID = "node-5555"
	moved.Nodes = []string{
		"https://cp/api/v2/clusters/" + opsClusterID + "/storage-nodes/node-5555/",
		"https://cp/api/v2/clusters/" + opsClusterID + "/storage-nodes/node-3333/",
	}
	api := aControlPlane().
		withPeer(opsPeerID, nodeStatusOnline).
		withPeer("node-3333", nodeStatusOnline).
		withPeer("node-4444", nodeStatusOnline).
		holding(publishedUnder(replicatedOn("volume-1", "pvc-1", opsPeerID), subsystemA), moved)
	mover := &scriptedMover{}
	r, _ := aDraining(t, api, mover,
		aPersistentVolume("pv-1", "volume-1"), aClaim("pv-1", false),
		aPersistentVolume("pv-2", "volume-2"), aClaim("pv-2", false))

	if _, err := performing(t, r, aDrain(), stepMigratingVolumes); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if len(mover.started) != 1 || mover.started[0].TargetNodeUUID != "node-4444" {
		t.Errorf("moves raised = %+v, want one headed for node-4444, the peer holding a replica of "+
			"no member: node-3333 holds pvc-2's", mover.started)
	}
}
