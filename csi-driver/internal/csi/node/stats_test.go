// The redirect-to-active-volume walk: how a NodeStage/NodeGetVolumeStats call
// carrying a volume handle whose own lvol record is gone finds the volume that
// actually serves the data, across one fail-over or a whole relocate round
// trip's chain of them.
package node

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// fakeRelationshipAPI stubs exactly the two ClusterAPI calls the redirect
// walk makes; every other method panics via the embedded nil interface,
// which is the point -- the walk must touch nothing else.
type fakeRelationshipAPI struct {
	controlplane.ClusterAPI
	rels map[string]*controlplane.ReplicationRelationship
	conn map[string]map[string]string
}

func (f *fakeRelationshipAPI) GetRelationship(
	_ context.Context, lvolID string,
) (*controlplane.ReplicationRelationship, error) {
	rel, ok := f.rels[lvolID]
	if !ok {
		return nil, errors.New("no replication relationship")
	}
	return rel, nil
}

func (f *fakeRelationshipAPI) VolumeInfo(_ context.Context, lvolID, _ string) (map[string]string, error) {
	c, ok := f.conn[lvolID]
	if !ok {
		return nil, controlplane.ErrVolumeNotFound
	}
	return c, nil
}

// Regression: 2026-09-24-chained-relationship-resolves-one-hop-short — after
// a relocate ROUND TRIP the chain is original(A) -> hop-1 clone(B) -> hop-2
// clone(A, the live volume, named by active_lvol_id on every record). The
// redirect used the FIRST record's active_lvol_id but paired it with that
// same record's target cluster/pool -- fields describing a DIFFERENT hop --
// and asked cluster B for a volume that lives on cluster A ("volume not
// found," confirmed live 2026-09-24), then fell back to the stale stashed
// context and the mount timed out. The walk must follow each record's own
// consistent target triple, hop by hop, until the hop whose target IS the
// active volume.
func TestRedirectToActiveVolumeWalksAChainedRelationship(t *testing.T) {
	const (
		clusterA = "aaaaaaaa-0000-0000-0000-000000000001"
		clusterB = "bbbbbbbb-0000-0000-0000-000000000001"
		poolA    = "aaaaaaaa-0000-0000-0000-00000000000a"
		poolB    = "bbbbbbbb-0000-0000-0000-00000000000b"
		original = "11111111-1111-1111-1111-111111111111"
		hop1     = "22222222-2222-2222-2222-222222222222"
		active   = "33333333-3333-3333-3333-333333333333"
	)

	srcClient := &fakeRelationshipAPI{
		rels: map[string]*controlplane.ReplicationRelationship{
			original: {
				SourceLvolID: original, TargetLvolID: hop1,
				SourceClusterID: clusterA, TargetClusterID: clusterB, TargetPoolID: poolB,
				ActiveLvolID: active,
			},
		},
	}
	clusterBClient := &fakeRelationshipAPI{
		rels: map[string]*controlplane.ReplicationRelationship{
			hop1: {
				SourceLvolID: hop1, TargetLvolID: active,
				SourceClusterID: clusterB, TargetClusterID: clusterA, TargetPoolID: poolA,
				ActiveLvolID: active,
			},
		},
	}
	clusterAClient := &fakeRelationshipAPI{
		conn: map[string]map[string]string{
			active: {"nqn": "nqn.test:" + active, "ip": "10.0.0.1", "port": "4420"},
		},
	}

	orig := clusterClientFor
	defer func() { clusterClientFor = orig }()
	clusterClientFor = func(_ context.Context, clusterID, poolID string) (controlplane.ClusterAPI, error) {
		switch clusterID + "/" + poolID {
		case clusterB + "/" + poolB:
			return clusterBClient, nil
		case clusterA + "/" + poolA:
			return clusterAClient, nil
		}
		return nil, errors.New("unexpected cluster " + clusterID + "/" + poolID)
	}

	connInfo := redirectToActiveVolume(context.Background(), srcClient, original,
		clusterA+":"+poolA+":"+original, map[string]string{"hostNQN": "nqn.host"})
	if connInfo == nil {
		t.Fatal("redirect returned nil: the walk never reached the active volume")
	}
	if got := connInfo["nqn"]; got != "nqn.test:"+active {
		t.Errorf("connection nqn = %q, want the ACTIVE volume's %q", got, "nqn.test:"+active)
	}
	if got := connInfo[csicommon.ParamClusterID]; got != clusterA {
		t.Errorf("cluster_id = %q, want the active volume's cluster %q, not the first hop's", got, clusterA)
	}
	if got := connInfo["poolID"]; got != poolA {
		t.Errorf("poolID = %q, want the active volume's pool %q", got, poolA)
	}
}

// The single-pairing case the redirect was originally written for (a
// migration with --delete-source, or one fail-over): the first record's
// target IS the active volume, and the walk must behave exactly as the
// one-step redirect always did.
func TestRedirectToActiveVolumeSinglePairingIsUnchanged(t *testing.T) {
	const (
		clusterA = "aaaaaaaa-0000-0000-0000-000000000001"
		clusterB = "bbbbbbbb-0000-0000-0000-000000000001"
		poolB    = "bbbbbbbb-0000-0000-0000-00000000000b"
		original = "11111111-1111-1111-1111-111111111111"
		active   = "22222222-2222-2222-2222-222222222222"
	)

	srcClient := &fakeRelationshipAPI{
		rels: map[string]*controlplane.ReplicationRelationship{
			original: {
				SourceLvolID: original, TargetLvolID: active,
				SourceClusterID: clusterA, TargetClusterID: clusterB, TargetPoolID: poolB,
				ActiveLvolID: active,
			},
		},
	}
	clusterBClient := &fakeRelationshipAPI{
		conn: map[string]map[string]string{
			active: {"nqn": "nqn.test:" + active},
		},
	}

	orig := clusterClientFor
	defer func() { clusterClientFor = orig }()
	clusterClientFor = func(_ context.Context, clusterID, poolID string) (controlplane.ClusterAPI, error) {
		if clusterID == clusterB && poolID == poolB {
			return clusterBClient, nil
		}
		return nil, errors.New("unexpected cluster " + clusterID)
	}

	connInfo := redirectToActiveVolume(context.Background(), srcClient, original,
		clusterA+":pool:"+original, map[string]string{})
	if connInfo == nil {
		t.Fatal("redirect returned nil for the plain single-pairing case")
	}
	if got := connInfo[csicommon.ParamClusterID]; got != clusterB {
		t.Errorf("cluster_id = %q, want %q", got, clusterB)
	}
}

// A volume moved many times: the PV keeps the original handle while every
// relocate and fail-over appends a clone, alternating between the two
// clusters, so the live copy sits one hop further out after each move. The
// walk must reach it however long the chain has grown; a cap of 8 stranded
// the ninth move's clone and the node attached the original on the
// partitioned site instead (2026-10-03).
func TestRedirectToActiveVolumeFollowsALongChain(t *testing.T) {
	const (
		clusterA = "aaaaaaaa-0000-0000-0000-000000000001"
		clusterB = "bbbbbbbb-0000-0000-0000-000000000001"
		poolA    = "aaaaaaaa-0000-0000-0000-00000000000a"
		poolB    = "bbbbbbbb-0000-0000-0000-00000000000b"
		moves    = 12
	)
	member := func(i int) string { return fmt.Sprintf("%08d-0000-0000-0000-000000000000", i) }
	cluster := func(i int) (string, string) {
		if i%2 == 0 {
			return clusterA, poolA
		}
		return clusterB, poolB
	}
	active := member(moves)
	clients := map[string]*fakeRelationshipAPI{
		clusterA + "/" + poolA: {
			rels: map[string]*controlplane.ReplicationRelationship{},
			conn: map[string]map[string]string{},
		},
		clusterB + "/" + poolB: {
			rels: map[string]*controlplane.ReplicationRelationship{},
			conn: map[string]map[string]string{},
		},
	}
	for i := 0; i < moves; i++ {
		srcC, srcP := cluster(i)
		tgtC, tgtP := cluster(i + 1)
		clients[srcC+"/"+srcP].rels[member(i)] = &controlplane.ReplicationRelationship{
			SourceLvolID: member(i), TargetLvolID: member(i + 1),
			SourceClusterID: srcC, TargetClusterID: tgtC, TargetPoolID: tgtP,
			ActiveLvolID: active,
		}
	}
	activeC, activeP := cluster(moves)
	clients[activeC+"/"+activeP].conn[active] = map[string]string{"nqn": "nqn.test:" + active}

	orig := clusterClientFor
	defer func() { clusterClientFor = orig }()
	clusterClientFor = func(_ context.Context, clusterID, poolID string) (controlplane.ClusterAPI, error) {
		if c, ok := clients[clusterID+"/"+poolID]; ok {
			return c, nil
		}
		return nil, errors.New("unexpected cluster " + clusterID + "/" + poolID)
	}

	connInfo := redirectToActiveVolume(context.Background(), clients[clusterA+"/"+poolA], member(0),
		clusterA+":"+poolA+":"+member(0), map[string]string{"hostNQN": "nqn.host"})
	if connInfo == nil {
		t.Fatalf("redirect returned nil: the walk gave up before the %d-hop chain's active volume", moves)
	}
	if got := connInfo["nqn"]; got != "nqn.test:"+active {
		t.Errorf("connection nqn = %q, want the active volume's %q", got, "nqn.test:"+active)
	}
	if got := connInfo[csicommon.ParamClusterID]; got != activeC {
		t.Errorf("cluster_id = %q, want the active volume's cluster %q", got, activeC)
	}
}

// A relationship that points back at a member already walked must end the
// walk instead of spinning to the bound.
func TestRedirectToActiveVolumeStopsOnALoop(t *testing.T) {
	const (
		clusterA = "aaaaaaaa-0000-0000-0000-000000000001"
		poolA    = "aaaaaaaa-0000-0000-0000-00000000000a"
		x        = "11111111-1111-1111-1111-111111111111"
		y        = "22222222-2222-2222-2222-222222222222"
	)
	client := &fakeRelationshipAPI{rels: map[string]*controlplane.ReplicationRelationship{
		x: {
			SourceLvolID: x, TargetLvolID: y, SourceClusterID: clusterA,
			TargetClusterID: clusterA, TargetPoolID: poolA, ActiveLvolID: "zz",
		},
		y: {
			SourceLvolID: y, TargetLvolID: x, SourceClusterID: clusterA,
			TargetClusterID: clusterA, TargetPoolID: poolA, ActiveLvolID: "zz",
		},
	}}
	orig := clusterClientFor
	defer func() { clusterClientFor = orig }()
	clusterClientFor = func(context.Context, string, string) (controlplane.ClusterAPI, error) { return client, nil }
	got := redirectToActiveVolume(context.Background(), client, x, clusterA+":"+poolA+":"+x, map[string]string{})
	if got != nil {
		t.Fatalf("a looping chain returned %v, want nil", got)
	}
}
