package controller

import (
	"context"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The Replication verbs route a cg: group handle to the group-replication
// endpoints, driving the whole consistency group as one unit (design §14.4). A
// per-volume handle still takes the §5 path (covered by replication_test.go).

const (
	vgGroupHandle = "cg:" + sanityClusterID + ":" + vgGroupID
	vgPolicyID    = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
)

// groupSource builds the ReplicationSource the csi-addons sidecar actually sends
// when driving a VolumeGroupReplication: the group handle rides the volumegroup
// oneof, NOT the per-volume one. Using the volume oneof here (as this helper
// once did) exercised the same wrong field the code read, so every group-routing
// test passed while live VGR promote failed with `invalid volume handle ""`
// (2026-09-26). Regression: 2026-09-26-vgr-source-oneof.
func groupSource() *replication.ReplicationSource {
	return &replication.ReplicationSource{
		Type: &replication.ReplicationSource_Volumegroup{
			Volumegroup: &replication.ReplicationSource_VolumeGroupSource{VolumeGroupId: vgGroupHandle},
		},
	}
}

func newGroupReplTestServer(t *testing.T, mock *mockSBCLI) *Server {
	t.Helper()
	mock.seedGroup(vgGroupID, vgMember1, vgMember2)
	return newTestControllerServer(t, mock)
}

func TestEnableVolumeReplicationRoutesAGroupHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		ReplicationSource: groupSource(),
		Parameters:        map[string]string{replicationPolicyParam: vgPolicyID},
	})
	if err != nil {
		t.Fatalf("EnableVolumeReplication: %v", err)
	}
	if got := mock.groups[vgGroupID].PolicyID; got != vgPolicyID {
		t.Fatalf("group policy = %q, want %q", got, vgPolicyID)
	}
}

// The group fail-over target has no reverse policy on its VolumeGroupReplication
// class either, so Enable on the cg: handle must be a no-op there; PromoteGroup
// clones and reconstitutes the group. Regression: 2026-09-27-failover-empty-policy.
func TestEnableVolumeReplicationEmptyPolicyIsNoOpForGroup(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)

	_, err := cs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		ReplicationSource: groupSource(),
		Parameters:        map[string]string{},
	})
	if err != nil {
		t.Fatalf("empty policy should be a no-op on the group fail-over target, got: %v", err)
	}
	if got := mock.groups[vgGroupID].PolicyID; got != "" {
		t.Fatalf("group policy = %q, want empty (nothing attached when no policy)", got)
	}
}

func TestDisableVolumeReplicationRoutesAGroupHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)
	mock.groups[vgGroupID].PolicyID = vgPolicyID

	_, err := cs.DisableVolumeReplication(context.Background(), &replication.DisableVolumeReplicationRequest{
		ReplicationSource: groupSource(),
	})
	if err != nil {
		t.Fatalf("DisableVolumeReplication: %v", err)
	}
	if got := mock.groups[vgGroupID].PolicyID; got != "" {
		t.Fatalf("group policy = %q after disable, want empty", got)
	}
}

func TestPromoteVolumeRoutesAGroupHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)

	if _, err := cs.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: groupSource(), Force: true,
	}); err != nil {
		t.Fatalf("PromoteVolume: %v", err)
	}
	if !mock.groups[vgGroupID].Promoted {
		t.Fatal("group was not promoted")
	}
}

func TestDemoteVolumeRoutesAGroupHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)

	if _, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		ReplicationSource: groupSource(),
	}); err != nil {
		t.Fatalf("DemoteVolume: %v", err)
	}
	if !mock.groups[vgGroupID].Demoted {
		t.Fatal("group was not demoted")
	}
}

func TestDemoteVolumeGroupStillConvergingIsAborted(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)
	mock.groups[vgGroupID].DemoteConverging = true

	_, err := cs.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		ReplicationSource: groupSource(),
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("err = %v, want Aborted", err)
	}
}

func TestResyncVolumeRoutesAGroupHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)

	resp, err := cs.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
		ReplicationSource: groupSource(),
		Parameters:        map[string]string{sourceClusterIDParam: sanityClusterID},
	})
	if err != nil {
		t.Fatalf("ResyncVolume: %v", err)
	}
	if !resp.GetReady() {
		t.Error("expected Ready (group status has no lag budget, so ready)")
	}
	if got := mock.groups[vgGroupID].FailbackSource; got != sanityClusterID {
		t.Fatalf("failback source = %q, want %q", got, sanityClusterID)
	}
}

func TestGetVolumeReplicationInfoRoutesAGroupHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)
	mock.groups[vgGroupID].LastReplicatedAt = 1_700_000_000

	resp, err := cs.GetVolumeReplicationInfo(context.Background(), &replication.GetVolumeReplicationInfoRequest{
		ReplicationSource: groupSource(),
	})
	if err != nil {
		t.Fatalf("GetVolumeReplicationInfo: %v", err)
	}
	if resp.GetLastSyncTime() == nil {
		t.Fatal("expected a LastSyncTime for the group")
	}
}
