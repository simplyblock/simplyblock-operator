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

func groupSource(handle string) *replication.ReplicationSource {
	return &replication.ReplicationSource{
		Type: &replication.ReplicationSource_Volume{
			Volume: &replication.ReplicationSource_VolumeSource{VolumeId: handle},
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
		ReplicationSource: groupSource(vgGroupHandle),
		Parameters:        map[string]string{replicationPolicyParam: vgPolicyID},
	})
	if err != nil {
		t.Fatalf("EnableVolumeReplication: %v", err)
	}
	if got := mock.groups[vgGroupID].PolicyID; got != vgPolicyID {
		t.Fatalf("group policy = %q, want %q", got, vgPolicyID)
	}
}

func TestDisableVolumeReplicationRoutesAGroupHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)
	mock.groups[vgGroupID].PolicyID = vgPolicyID

	_, err := cs.DisableVolumeReplication(context.Background(), &replication.DisableVolumeReplicationRequest{
		ReplicationSource: groupSource(vgGroupHandle),
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
		ReplicationSource: groupSource(vgGroupHandle), Force: true,
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
		ReplicationSource: groupSource(vgGroupHandle),
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
		ReplicationSource: groupSource(vgGroupHandle),
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
		ReplicationSource: groupSource(vgGroupHandle),
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
		ReplicationSource: groupSource(vgGroupHandle),
	})
	if err != nil {
		t.Fatalf("GetVolumeReplicationInfo: %v", err)
	}
	if resp.GetLastSyncTime() == nil {
		t.Fatal("expected a LastSyncTime for the group")
	}
}
