package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// handle builds a 3-part CSI volume handle for a member lvol.
func handle(lvolID string) string {
	return sanityClusterID + ":" + sanityPoolUUID + ":" + lvolID
}

// seedGroupServer wires a controller server to a mock backend holding one group
// with the given member lvol UUIDs, and returns the server, mock, and handles.
func seedGroupServer(t *testing.T, groupUUID string, memberUUIDs ...string) (*Server, *mockSBCLI, []string) {
	t.Helper()
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	mock.seedGroup(groupUUID, memberUUIDs...)
	cs := newTestControllerServer(t, mock)
	handles := make([]string, len(memberUUIDs))
	for i, id := range memberUUIDs {
		handles[i] = handle(id)
	}
	return cs, mock, handles
}

func TestGroupControllerGetCapabilities(t *testing.T) {
	cs := newTestControllerServer(t, func() *mockSBCLI { m := newMockSBCLI(); t.Cleanup(m.Close); return m }())
	resp, err := cs.GroupControllerGetCapabilities(context.Background(), &csi.GroupControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GroupControllerGetCapabilities: %v", err)
	}
	found := false
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT {
			found = true
		}
	}
	if !found {
		t.Fatal("CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT capability not advertised")
	}
}

func TestCreateVolumeGroupSnapshot_HandlesEqualMembership(t *testing.T) {
	a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cs, _, handles := seedGroupServer(t, uuid.NewString(), a, b, c)

	resp, err := cs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "gen",
		SourceVolumeIds: handles,
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot: %v", err)
	}
	gs := resp.GetGroupSnapshot()
	if n := strings.Count(gs.GetGroupSnapshotId(), ":"); n != 3 {
		t.Fatalf("group snapshot id %q: want 4 colon-separated parts, got %d", gs.GetGroupSnapshotId(), n+1)
	}
	if !gs.GetReadyToUse() {
		t.Error("group snapshot should be ready when every member snapshot is present")
	}
	if len(gs.GetSnapshots()) != 3 {
		t.Fatalf("want 3 member snapshots, got %d", len(gs.GetSnapshots()))
	}
	sources := map[string]bool{}
	for _, s := range gs.GetSnapshots() {
		if strings.Count(s.GetSnapshotId(), ":") != 2 {
			t.Errorf("member snapshot id %q is not a 3-part CSI id", s.GetSnapshotId())
		}
		sources[s.GetSourceVolumeId()] = true
	}
	for _, h := range handles {
		if !sources[h] {
			t.Errorf("source volume %s missing from response", h)
		}
	}
}

func TestCreateVolumeGroupSnapshot_ExtraHandleIsRefused(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	cs, _, handles := seedGroupServer(t, uuid.NewString(), a, b)
	handles = append(handles, handle(uuid.NewString())) // a non-member fourth handle

	_, err := cs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "gen",
		SourceVolumeIds: handles,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for a set larger than membership, got %v", err)
	}
}

func TestCreateVolumeGroupSnapshot_MissingHandleIsRefused(t *testing.T) {
	a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cs, _, handles := seedGroupServer(t, uuid.NewString(), a, b, c)

	_, err := cs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "gen",
		SourceVolumeIds: handles[:2], // one member short
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for a set smaller than membership, got %v", err)
	}
}

func TestCreateVolumeGroupSnapshot_NonMemberIsRefused(t *testing.T) {
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	orphan := uuid.NewString()
	mock.volumes[orphan] = &mockVolume{UUID: orphan, Name: orphan, Size: 1 << 30} // GroupID ""
	cs := newTestControllerServer(t, mock)

	_, err := cs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "gen",
		SourceVolumeIds: []string{handle(orphan)},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for a non-member volume, got %v", err)
	}
}

func TestCreateVolumeGroupSnapshot_TwoGroupsRefused(t *testing.T) {
	a1, a2 := uuid.NewString(), uuid.NewString()
	b1 := uuid.NewString()
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	mock.seedGroup(uuid.NewString(), a1, a2)
	mock.seedGroup(uuid.NewString(), b1)
	cs := newTestControllerServer(t, mock)

	// The set {a1, b1} spans two groups: it never equals group A's membership.
	_, err := cs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "gen",
		SourceVolumeIds: []string{handle(a1), handle(b1)},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for a cross-group selector, got %v", err)
	}
}

func TestDeleteVolumeGroupSnapshot_MissingIsSuccess(t *testing.T) {
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	cs := newTestControllerServer(t, mock)

	// group snapshot id naming a group that no longer exists
	gsID := sanityClusterID + ":" + sanityPoolUUID + ":" + uuid.NewString() + ":1"
	_, err := cs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: gsID,
	})
	if err != nil {
		t.Fatalf("delete of a missing group snapshot should succeed, got %v", err)
	}
}

func TestDeleteVolumeGroupSnapshot_UnparseableIsSuccess(t *testing.T) {
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	cs := newTestControllerServer(t, mock)
	_, err := cs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: "not-a-valid-id",
	})
	if err != nil {
		t.Fatalf("delete of an unparsable id should succeed, got %v", err)
	}
}

func TestGetVolumeGroupSnapshot_ReadsGeneration(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	cs, _, handles := seedGroupServer(t, uuid.NewString(), a, b)

	created, err := cs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "gen",
		SourceVolumeIds: handles,
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot: %v", err)
	}
	gsID := created.GetGroupSnapshot().GetGroupSnapshotId()

	got, err := cs.GetVolumeGroupSnapshot(context.Background(), &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: gsID,
	})
	if err != nil {
		t.Fatalf("GetVolumeGroupSnapshot: %v", err)
	}
	if got.GetGroupSnapshot().GetGroupSnapshotId() != gsID {
		t.Errorf("group snapshot id round-trip: want %s, got %s", gsID, got.GetGroupSnapshot().GetGroupSnapshotId())
	}
	if len(got.GetGroupSnapshot().GetSnapshots()) != 2 {
		t.Errorf("want 2 member snapshots, got %d", len(got.GetGroupSnapshot().GetSnapshots()))
	}
}

func TestCreateVolumeGroupSnapshot_BackendTakeErrorSurfaces(t *testing.T) {
	a := uuid.NewString()
	cs, mock, handles := seedGroupServer(t, uuid.NewString(), a)
	// The take POST fails (e.g., an unhealthy member the precheck rejects).
	mock.injectStatus = func(r *http.Request) int {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/consistency-groups/") {
			return http.StatusUnprocessableEntity
		}
		return 0
	}
	_, err := cs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "gen",
		SourceVolumeIds: handles,
	})
	if err == nil {
		t.Fatal("expected an error when the backend refuses the group snapshot")
	}
}

func TestCreateVolumeGroupSnapshot_RejectsEmptyRequest(t *testing.T) {
	mock := newMockSBCLI()
	t.Cleanup(mock.Close)
	cs := newTestControllerServer(t, mock)
	_, err := cs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{Name: "gen"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for no source volumes, got %v", err)
	}
}
