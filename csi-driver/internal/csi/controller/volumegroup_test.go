package controller

import (
	"context"
	"testing"

	"github.com/csi-addons/spec/lib/go/volumegroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	vgGroupID = "c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9"
	vgMember1 = "a1111111-1111-4111-8111-111111111111"
	vgMember2 = "b2222222-2222-4222-8222-222222222222"
)

func vgHandle(volumeID string) string {
	return sanityClusterID + ":" + sanityPoolUUID + ":" + volumeID
}

// CreateVolumeGroup resolves the backend consistency group its member volumes
// already belong to, and returns that group's handle (design §14.3).
func TestCreateVolumeGroupResolvesTheBackendGroup(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.seedGroup(vgGroupID, vgMember1, vgMember2)
	cs := newTestControllerServer(t, mock)

	resp, err := cs.CreateVolumeGroup(context.Background(), &volumegroup.CreateVolumeGroupRequest{
		Name:      "vgrcontent-generated-name",
		VolumeIds: []string{vgHandle(vgMember1), vgHandle(vgMember2)},
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroup: %v", err)
	}
	want := "cg:" + sanityClusterID + ":" + vgGroupID
	if got := resp.GetVolumeGroup().GetVolumeGroupId(); got != want {
		t.Fatalf("group handle = %q, want %q", got, want)
	}
}

// A selection that is not exactly one backend group's membership must not be
// silently grouped.
func TestCreateVolumeGroupRefusesAPartialSelection(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	mock.seedGroup(vgGroupID, vgMember1, vgMember2)
	cs := newTestControllerServer(t, mock)

	_, err := cs.CreateVolumeGroup(context.Background(), &volumegroup.CreateVolumeGroupRequest{
		VolumeIds: []string{vgHandle(vgMember1)}, // only one of the two members
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestCreateVolumeGroupRejectsAMalformedHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	_, err := cs.CreateVolumeGroup(context.Background(), &volumegroup.CreateVolumeGroupRequest{
		VolumeIds: []string{"not-a-volume-handle"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

func TestCreateVolumeGroupRequiresAVolume(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	_, err := cs.CreateVolumeGroup(context.Background(), &volumegroup.CreateVolumeGroupRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

// ModifyVolumeGroupMembership is a success no-op (membership is label-driven,
// design §14.3), so the controller-manager's teardown does not error.
func TestModifyVolumeGroupMembershipIsANoOpSuccess(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	gh := "cg:" + sanityClusterID + ":" + vgGroupID
	resp, err := cs.ModifyVolumeGroupMembership(context.Background(),
		&volumegroup.ModifyVolumeGroupMembershipRequest{VolumeGroupId: gh})
	if err != nil {
		t.Fatalf("ModifyVolumeGroupMembership: %v", err)
	}
	if got := resp.GetVolumeGroup().GetVolumeGroupId(); got != gh {
		t.Fatalf("group handle = %q, want %q", got, gh)
	}
}

// DeleteVolumeGroup is a success no-op: the backend group is label-formed and
// survives the csi-addons grouping (design §14.3).
func TestDeleteVolumeGroupIsANoOpSuccess(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	gh := "cg:" + sanityClusterID + ":" + vgGroupID
	if _, err := cs.DeleteVolumeGroup(context.Background(),
		&volumegroup.DeleteVolumeGroupRequest{VolumeGroupId: gh}); err != nil {
		t.Fatalf("DeleteVolumeGroup: %v", err)
	}
}

func TestVolumeGroupVerbsRejectANonGroupHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	perVolume := vgHandle(vgMember1) // a per-volume handle, not a group handle

	if _, err := cs.ModifyVolumeGroupMembership(context.Background(),
		&volumegroup.ModifyVolumeGroupMembershipRequest{VolumeGroupId: perVolume}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Modify: err = %v, want InvalidArgument", err)
	}
	if _, err := cs.DeleteVolumeGroup(context.Background(),
		&volumegroup.DeleteVolumeGroupRequest{VolumeGroupId: perVolume}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Delete: err = %v, want InvalidArgument", err)
	}
}
