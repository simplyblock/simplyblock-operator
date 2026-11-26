// Tests for the group promote on a split control plane: a group handle that
// names a cluster on another control plane (a Peer entry in the secret, or no
// entry at all) is failed over through this site's own control plane, from the
// copies there, instead of through the other site's, which may be cut off.
package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/csi-driver/internal/clusters"
)

// siteBClusterID is the cluster of this site's own control plane; the group
// handle names sanityClusterID, the source cluster on the other site's.
const siteBClusterID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

// writeSecret writes the driver's secret with the given entries.
func writeSecret(t *testing.T, entries ...clusters.Config) {
	t.Helper()
	secretFile := filepath.Join(t.TempDir(), "secret.json")
	data, _ := json.Marshal(clusters.Info{Clusters: entries})
	if err := os.WriteFile(secretFile, data, 0600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("SPDKCSI_SECRET", secretFile)
}

// splitSite returns a server on site B whose secret holds site B's own
// cluster (local, served by local) and, unless sourceEntry is false, the
// source cluster as a Peer entry whose control plane is down.
func splitSite(t *testing.T, local *mockSBCLI, sourceEntry bool) *Server {
	t.Helper()
	cs := newTestControllerServer(t, local)
	down := newMockSBCLI()
	downURL := down.URL()
	down.Close()
	entries := []clusters.Config{{
		ClusterID: siteBClusterID, ClusterEndpoint: local.URL(), ClusterSecret: sanitySecret, Local: true,
	}}
	if sourceEntry {
		entries = append(entries, clusters.Config{
			ClusterID: sanityClusterID, ClusterEndpoint: downURL, ClusterSecret: sanitySecret, Peer: true,
		})
	}
	writeSecret(t, entries...)
	return cs
}

func promoteGroup(cs *Server) error {
	_, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: groupSource(), Force: true,
	})
	return err
}

func TestPromoteOfAPeerClustersGroupFailsItOverOnThisSite(t *testing.T) {
	local := newMockSBCLI()
	defer local.Close()
	cs := splitSite(t, local, true)

	if err := promoteGroup(cs); err != nil {
		t.Fatalf("PromoteVolume: %v", err)
	}
	want := siteBClusterID + "/" + sanityClusterID + "/" + vgGroupID
	if len(local.replicatedFailovers) != 1 || local.replicatedFailovers[0] != want {
		t.Fatalf("fail-overs on this site = %v, want [%s]", local.replicatedFailovers, want)
	}
}

func TestPromoteOfAGroupOnAClusterTheSecretDoesNotListFailsItOverOnThisSite(t *testing.T) {
	local := newMockSBCLI()
	defer local.Close()
	cs := splitSite(t, local, false)

	if err := promoteGroup(cs); err != nil {
		t.Fatalf("PromoteVolume: %v", err)
	}
	if len(local.replicatedFailovers) != 1 {
		t.Fatalf("fail-overs on this site = %v, want one", local.replicatedFailovers)
	}
}

func TestPromoteOnThisSiteWithNoCompleteGenerationIsRetryable(t *testing.T) {
	local := newMockSBCLI()
	defer local.Close()
	local.replicatedFailoverStatus = 409
	cs := splitSite(t, local, true)

	err := promoteGroup(cs)
	if status.Code(err) != codes.Aborted {
		t.Fatalf("PromoteVolume error = %v, want Aborted (retryable)", err)
	}
}

func TestPromoteOfALocalClustersGroupKeepsTheGroupPromote(t *testing.T) {
	// One control plane for both sites: the source cluster's entry is not a
	// Peer, so its own control plane resolves the promote as before.
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)

	if err := promoteGroup(cs); err != nil {
		t.Fatalf("PromoteVolume: %v", err)
	}
	if !mock.groups[vgGroupID].Promoted || len(mock.replicatedFailovers) != 0 {
		t.Fatalf("promoted=%v replicated fail-overs=%v, want the group promote only",
			mock.groups[vgGroupID].Promoted, mock.replicatedFailovers)
	}
}
