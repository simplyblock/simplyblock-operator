// The redirect-to-active-volume walk: how a NodeStage/NodeGetVolumeStats call
// carrying a volume handle whose own lvol record is gone finds the volume that
// actually serves the data, across one fail-over or a whole relocate round
// trip's chain of them.
package node

import (
	"context"
	"errors"
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
