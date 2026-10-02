package controller

import (
	"testing"

	"github.com/simplyblock/atlas/lvol"
)

// The chain of 2026-10-02 (realbed, WordPress): created on A (0aea, gone),
// failed over to B (6e83), relocated back to A (80e3), failed over to B
// (e3d4, the live primary). Ramen then made the old primary on A secondary.
func liveChain() []chainHop {
	mk := func(cluster, id string) chainHop { return chainHop{h: &lvol.Handle{ClusterID: cluster, PoolRef: "p", VolumeID: id}} }
	return []chainHop{mk("A", "0aea"), mk("B", "6e83"), mk("A", "80e3"), mk("B", "e3d4")}
}

func TestChooseReplicaOnTheOldPrimarysSiteIsTheOldPrimaryNotTheLivePrimary(t *testing.T) {
	got := chooseReplica(liveChain(), map[string]bool{"A": true}, true)
	if got.h.VolumeID != "80e3" {
		t.Fatalf("site A acts on %s, want 80e3 (its own, superseded primary); e3d4 is the live primary on B", got.h.VolumeID)
	}
}

func TestChooseReplicaOnTheNewPrimarysSiteIsTheLivePrimary(t *testing.T) {
	got := chooseReplica(liveChain(), map[string]bool{"B": true}, true)
	if got.h.VolumeID != "e3d4" {
		t.Fatalf("site B acts on %s, want e3d4", got.h.VolumeID)
	}
}

func TestChooseReplicaWithoutLocalFlagsKeepsTheChainsEnd(t *testing.T) {
	got := chooseReplica(liveChain(), nil, false)
	if got.h.VolumeID != "e3d4" {
		t.Fatalf("unflagged secret: %s, want the chain's end e3d4", got.h.VolumeID)
	}
}

func TestChooseReplicaWithNoLocalMemberKeepsTheChainsEnd(t *testing.T) {
	got := chooseReplica(liveChain(), map[string]bool{"C": true}, true)
	if got.h.VolumeID != "e3d4" {
		t.Fatalf("no member on C: %s, want the chain's end e3d4", got.h.VolumeID)
	}
}

func TestChooseReplicaOfAVolumeWithoutARelationshipIsTheVolume(t *testing.T) {
	one := liveChain()[:1]
	if got := chooseReplica(one, map[string]bool{"B": true}, true); got.h.VolumeID != "0aea" {
		t.Fatalf("got %s", got.h.VolumeID)
	}
}
