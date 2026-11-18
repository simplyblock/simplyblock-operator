// Tests for staging a failed-over volume on a split control plane: the PV's
// handle names a cluster on the other site's control plane, which may be cut
// off, and this site's own control plane holds the relationship the fail-over
// recorded and the clone that serves the data.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

const (
	peerCluster  = "bbbbbbbb-1111-1111-1111-000000000001" // the source, on the other site
	localCluster = "aaaaaaaa-1111-1111-1111-000000000001" // this site's
	localPool    = "aaaaaaaa-1111-1111-1111-00000000000a"
	sourceVolume = "11111111-aaaa-aaaa-aaaa-111111111111"
	cloneVolume  = "22222222-aaaa-aaaa-aaaa-222222222222"
)

func writeNodeSecret(t *testing.T, entries ...clusters.Config) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "secret.json")
	data, _ := json.Marshal(clusters.Info{Clusters: entries})
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	t.Setenv("SPDKCSI_SECRET", file)
}

// localSite fakes this site's control plane: it knows the fail-over's
// relationship by the source volume's id, and serves the clone.
func localSite(t *testing.T) {
	t.Helper()
	local := &fakeRelationshipAPI{
		rels: map[string]*controlplane.ReplicationRelationship{
			sourceVolume: {
				SourceLvolID: sourceVolume, TargetLvolID: cloneVolume,
				SourceClusterID: peerCluster, TargetClusterID: localCluster, TargetPoolID: localPool,
				ActiveLvolID: cloneVolume,
			},
		},
		conn: map[string]map[string]string{
			cloneVolume: {"uuid": cloneVolume, "nqn": "nqn.test:" + cloneVolume},
		},
	}
	orig := clusterClientFor
	t.Cleanup(func() { clusterClientFor = orig })
	clusterClientFor = func(_ context.Context, clusterID, _ string) (controlplane.ClusterAPI, error) {
		if clusterID == localCluster {
			return local, nil
		}
		return nil, errors.New("the other site's control plane is cut off: " + clusterID)
	}
}

func TestAPeerClustersVolumeStagesFromItsCloneOnThisSite(t *testing.T) {
	writeNodeSecret(t,
		clusters.Config{ClusterID: localCluster, ClusterEndpoint: "http://local", Local: true},
		clusters.Config{ClusterID: peerCluster, ClusterEndpoint: "http://peer", Peer: true})
	localSite(t)

	connInfo := redirectThroughLocalCluster(context.Background(), peerCluster, sourceVolume,
		peerCluster+":pool:"+sourceVolume, map[string]string{"hostNQN": "nqn.host"})

	if connInfo == nil {
		t.Fatal("no redirect: the clone on this site was not found")
	}
	if connInfo["uuid"] != cloneVolume || connInfo[csicommon.ParamClusterID] != localCluster ||
		connInfo["poolID"] != localPool {
		t.Errorf("redirect = %v, want the clone %s on %s/%s", connInfo, cloneVolume, localCluster, localPool)
	}
}

func TestAVolumeOfAClusterTheSecretDoesNotListStagesFromItsCloneOnThisSite(t *testing.T) {
	writeNodeSecret(t, clusters.Config{ClusterID: localCluster, ClusterEndpoint: "http://local", Local: true})
	localSite(t)

	connInfo := redirectThroughLocalCluster(context.Background(), peerCluster, sourceVolume,
		peerCluster+":pool:"+sourceVolume, map[string]string{"hostNQN": "nqn.host"})

	if connInfo == nil || connInfo["uuid"] != cloneVolume {
		t.Fatalf("redirect = %v, want the clone %s", connInfo, cloneVolume)
	}
}

func TestALocalClustersVolumeIsNotRedirected(t *testing.T) {
	// One control plane for both sites, or the volume's own site: the handle's
	// cluster is not a Peer, and the usual path resolves it.
	writeNodeSecret(t, clusters.Config{ClusterID: peerCluster, ClusterEndpoint: "http://cp"})
	localSite(t)

	if connInfo := redirectThroughLocalCluster(context.Background(), peerCluster, sourceVolume,
		peerCluster+":pool:"+sourceVolume, map[string]string{}); connInfo != nil {
		t.Fatalf("redirect = %v, want none", connInfo)
	}
}
