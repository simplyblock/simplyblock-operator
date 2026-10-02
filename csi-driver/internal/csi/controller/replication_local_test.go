package controller

import (
	"context"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
	"github.com/simplyblock/atlas/lvol"
)

// The chain of 2026-10-02 (realbed, WordPress): created on A (0aea, gone),
// failed over to B (6e83), relocated back to A (80e3), failed over to B
// (e3d4, the live primary). Ramen then made the old primary on A secondary.
const (
	siteA, siteB   = "A", "B"
	oldPrimaryOnA  = "80e3"
	livePrimaryOnB = "e3d4"
)

func liveChain() []chainHop {
	mk := func(cluster, id string) chainHop {
		return chainHop{h: &lvol.Handle{ClusterID: cluster, PoolRef: "p", VolumeID: id}}
	}
	return []chainHop{mk(siteA, "0aea"), mk(siteB, "6e83"), mk(siteA, oldPrimaryOnA), mk(siteB, livePrimaryOnB)}
}

func TestChooseReplicaOnTheOldPrimarysSiteIsTheOldPrimaryNotTheLivePrimary(t *testing.T) {
	got := chooseReplica(liveChain(), map[string]bool{siteA: true}, true)
	if got.h.VolumeID != oldPrimaryOnA {
		t.Fatalf("site A acts on %s, want 80e3 (its own, superseded primary); e3d4 is the live primary on B", got.h.VolumeID)
	}
}

func TestChooseReplicaOnTheNewPrimarysSiteIsTheLivePrimary(t *testing.T) {
	got := chooseReplica(liveChain(), map[string]bool{siteB: true}, true)
	if got.h.VolumeID != livePrimaryOnB {
		t.Fatalf("site B acts on %s, want e3d4", got.h.VolumeID)
	}
}

func TestChooseReplicaWithoutLocalFlagsKeepsTheChainsEnd(t *testing.T) {
	got := chooseReplica(liveChain(), nil, false)
	if got.h.VolumeID != livePrimaryOnB {
		t.Fatalf("unflagged secret: %s, want the chain's end e3d4", got.h.VolumeID)
	}
}

func TestChooseReplicaWithNoLocalMemberKeepsTheChainsEnd(t *testing.T) {
	got := chooseReplica(liveChain(), map[string]bool{"C": true}, true)
	if got.h.VolumeID != livePrimaryOnB {
		t.Fatalf("no member on C: %s, want the chain's end e3d4", got.h.VolumeID)
	}
}

func TestChooseReplicaOfAVolumeWithoutARelationshipIsTheVolume(t *testing.T) {
	one := liveChain()[:1]
	if got := chooseReplica(one, map[string]bool{siteB: true}, true); got.h.VolumeID != "0aea" {
		t.Fatalf("got %s", got.h.VolumeID)
	}
}

// The old primary of an unplanned fail-over is reaped by the control plane
// once its fail-over completed; Ramen still demotes it (and deletes its VR)
// when the site returns. Nothing is left to demote: success, not NotFound
// (live 2026-10-02: the VR on site A stayed Degraded on a 404).
func TestDemoteAndDisableOfAReapedChainMemberSucceed(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newReplicationTestServer(t, mock)
	gone := "99999999-aaaa-bbbb-cccc-dddddddddddd"
	mock.replicationRelationship[testReplVolumeID] = map[string]any{
		"replication_id":    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"direction":         "to_target",
		"mode":              "failover",
		"state":             "failed_over",
		"is_source":         true,
		"source_cluster_id": sanityClusterID, "source_lvol_id": testReplVolumeID,
		"target_cluster_id": sanityClusterID, "target_pool_id": sanityPoolUUID, "target_lvol_id": gone,
		"active_lvol_id": gone,
		"target_nqn":     "nqn.test", "target_ns_id": 1,
	}
	if _, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{VolumeId: testReplVolID}); err != nil {
		t.Fatalf("demote of a reaped chain member: %v", err)
	}
	if _, err := cs.DisableVolumeReplication(context.Background(), &replication.DisableVolumeReplicationRequest{VolumeId: testReplVolID}); err != nil {
		t.Fatalf("disable of a reaped chain member: %v", err)
	}
}
