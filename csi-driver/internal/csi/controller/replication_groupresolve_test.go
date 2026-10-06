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

	atlascp "github.com/simplyblock/atlas/controlplane"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/csi-driver/internal/clusters"
)

// A VolumeGroupReplication keeps its original group handle across a relocate,
// while the group it names is emptied by design (its demoted members are
// deleted so a relocate back stays possible) and the data lives in the peer
// group of the same name on the other site. The group verbs resolve the handle
// through the control plane (2026-10-04, WordPress A -> B: Ramen's VRG waited
// for destination info for ever against the emptied source group).

const (
	peerClusterID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	peerGroupID   = "e5e5e5e5-e5e5-4e5e-8e5e-e5e5e5e5e5e5"
	peerClone     = "f6666666-6666-4666-8666-666666666666"
	origPV        = sanityClusterID + ":p-a:" + vgMember1
)

// writeTwoSiteSecret registers the named cluster (site A) and its peer (site
// B), both answered by the one mock, with site B flagged local: the driver runs
// on site B, where the relocate landed.
func writeTwoSiteSecret(t *testing.T, mock *mockSBCLI, localCluster string) {
	t.Helper()
	data, _ := json.Marshal(clusters.Info{Clusters: []clusters.Config{
		{ClusterID: sanityClusterID, ClusterEndpoint: mock.URL(), ClusterSecret: sanitySecret,
			Local: localCluster == sanityClusterID},
		{ClusterID: peerClusterID, ClusterEndpoint: mock.URL(), ClusterSecret: sanitySecret,
			Local: localCluster == peerClusterID},
	}})
	f := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(f, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPDKCSI_SECRET", f)
}

// movedGroup is the incident's state: the named group on A is empty, the data
// lives in the peer group on B as a clone of the original volume.
func movedGroup(t *testing.T, localCluster string) (*Server, *mockSBCLI) {
	t.Helper()
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	mock.seedGroup(vgGroupID)
	mock.seedGroup(peerGroupID, peerClone)
	mock.groupResolution[vgGroupID] = map[string]any{
		"cluster_id": sanityClusterID, "group_id": vgGroupID,
		"active_cluster_id": peerClusterID, "active_group_id": peerGroupID,
		"members": []map[string]string{{
			"origin_handle": origPV, "active_handle": peerClusterID + ":p-b:" + peerClone}},
	}
	cs := newTestControllerServer(t, mock)
	writeTwoSiteSecret(t, mock, localCluster)
	return cs, mock
}

func TestDestinationInfoOfAGroupEmptiedByARelocateMapsItsPVs(t *testing.T) {
	cs, _ := movedGroup(t, peerClusterID)

	resp, err := cs.GetReplicationDestinationInfo(context.Background(),
		&replication.GetReplicationDestinationInfoRequest{ReplicationSource: groupSource()})
	if err != nil {
		t.Fatalf("GetReplicationDestinationInfo: %v", err)
	}
	vg := resp.GetReplicationDestination().GetVolumegroup()
	if vg.GetVolumeGroupId() != vgGroupHandle {
		t.Fatalf("destination group = %q, want the VGR's own handle %q", vg.GetVolumeGroupId(), vgGroupHandle)
	}
	if got := vg.GetVolumeIds(); len(got) != 1 || got[origPV] != origPV {
		t.Fatalf("map = %v, want the PV's original handle mapped to itself", got)
	}
}

func TestDestinationInfoOfAGroupWithNoLiveMemberIsUnavailable(t *testing.T) {
	cs, mock := movedGroup(t, peerClusterID)
	mock.groupResolution[vgGroupID] = map[string]any{"cluster_id": sanityClusterID, "group_id": vgGroupID}

	_, err := cs.GetReplicationDestinationInfo(context.Background(),
		&replication.GetReplicationDestinationInfoRequest{ReplicationSource: groupSource()})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (retryable, never a partial map)", status.Code(err))
	}
}

// The relocate back B -> A: Ramen demotes the VGR on B, which names the group
// on A. Site B must demote the group it serves -- the peer group -- not the
// empty one on A.
func TestDemoteOfAMovedGroupActsOnTheLocalGroup(t *testing.T) {
	cs, mock := movedGroup(t, peerClusterID)

	if _, err := cs.DemoteVolume(context.Background(),
		&replication.DemoteVolumeRequest{ReplicationSource: groupSource()}); err != nil {
		t.Fatalf("DemoteVolume: %v", err)
	}
	if !mock.groups[peerGroupID].Demoted || mock.groups[vgGroupID].Demoted {
		t.Fatalf("demoted: peer %v, named %v; want the peer group on site B only",
			mock.groups[peerGroupID].Demoted, mock.groups[vgGroupID].Demoted)
	}
}

// Site A, the old primary, demoting its side must stay on its own group.
func TestDemoteOnTheOldPrimarysSiteStaysOnItsOwnGroup(t *testing.T) {
	cs, mock := movedGroup(t, sanityClusterID)

	if _, err := cs.DemoteVolume(context.Background(),
		&replication.DemoteVolumeRequest{ReplicationSource: groupSource()}); err != nil {
		t.Fatalf("DemoteVolume: %v", err)
	}
	if mock.groups[peerGroupID].Demoted || !mock.groups[vgGroupID].Demoted {
		t.Fatal("site A demoted the live group on site B")
	}
}

// Re-protection B -> A: Enable on the VGR's handle attaches the live group.
func TestEnableOfAMovedGroupAttachesTheLiveGroup(t *testing.T) {
	cs, mock := movedGroup(t, peerClusterID)

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		ReplicationSource: groupSource(),
		Parameters:        map[string]string{replicationPolicyParam: vgPolicyID},
	})
	if err != nil {
		t.Fatalf("EnableVolumeReplication: %v", err)
	}
	if mock.groups[peerGroupID].PolicyID != vgPolicyID || mock.groups[vgGroupID].PolicyID != "" {
		t.Fatalf("policy: peer %q, named %q; want it on the live group only",
			mock.groups[peerGroupID].PolicyID, mock.groups[vgGroupID].PolicyID)
	}
}

func TestInfoOfAMovedGroupReadsTheLiveGroup(t *testing.T) {
	cs, mock := movedGroup(t, peerClusterID)
	mock.groups[peerGroupID].LastReplicatedAt = 1791105000

	resp, err := cs.GetVolumeReplicationInfo(context.Background(),
		&replication.GetVolumeReplicationInfoRequest{ReplicationSource: groupSource()})
	if err != nil {
		t.Fatalf("GetVolumeReplicationInfo: %v", err)
	}
	if resp.GetLastSyncTime().GetSeconds() != 1791105000 {
		t.Fatalf("last sync = %v, want the live group's", resp.GetLastSyncTime())
	}
}

// Promote stays on the named group: the control plane's group fail-over
// resolves the peer itself, and redirecting it would turn a fail-back into a
// no-op on the group being left.
func TestPromoteOfAMovedGroupStaysOnTheNamedGroup(t *testing.T) {
	cs, mock := movedGroup(t, peerClusterID)

	if _, err := cs.PromoteVolume(context.Background(),
		&replication.PromoteVolumeRequest{ReplicationSource: groupSource()}); err != nil {
		t.Fatalf("PromoteVolume: %v", err)
	}
	if !mock.groups[vgGroupID].Promoted || mock.groups[peerGroupID].Promoted {
		t.Fatal("promote did not reach the named group")
	}
}

// A group still live where it was created resolves to itself.
func TestAGroupLiveAtItsSourceIsActedOnAsNamed(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)
	mock.groupResolution[vgGroupID] = map[string]any{
		"cluster_id": sanityClusterID, "group_id": vgGroupID,
		"active_cluster_id": sanityClusterID, "active_group_id": vgGroupID,
		"members": []map[string]string{{"origin_handle": origPV, "active_handle": origPV}},
	}
	if _, err := cs.DemoteVolume(context.Background(),
		&replication.DemoteVolumeRequest{ReplicationSource: groupSource()}); err != nil {
		t.Fatalf("DemoteVolume: %v", err)
	}
	if !mock.groups[vgGroupID].Demoted {
		t.Fatal("the named, live group was not demoted")
	}
}

func TestChooseGroup(t *testing.T) {
	named := lvol.GroupHandle{ClusterID: sanityClusterID, GroupID: vgGroupID}
	live := lvol.GroupHandle{ClusterID: peerClusterID, GroupID: peerGroupID}
	moved := atlascp.GroupResolution{Active: &live}
	for _, tc := range []struct {
		name  string
		res   atlascp.GroupResolution
		side  groupSide
		local map[string]bool
		want  lvol.GroupHandle
	}{
		{"not moved", atlascp.GroupResolution{Active: &named}, groupLocalSite, map[string]bool{peerClusterID: true}, named},
		{"nothing live", atlascp.GroupResolution{}, groupActiveEnd, nil, named},
		{"active end", moved, groupActiveEnd, map[string]bool{sanityClusterID: true}, live},
		{"local on the live site", moved, groupLocalSite, map[string]bool{peerClusterID: true}, live},
		{"local on the named site", moved, groupLocalSite, map[string]bool{sanityClusterID: true}, named},
		{"no local flags", moved, groupLocalSite, nil, live},
	} {
		if got := chooseGroup(named, tc.res, tc.side, tc.local); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got.Handle(), tc.want.Handle())
		}
	}
}
