package controller

import (
	"context"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func volumeSource(id string) *replication.ReplicationSource {
	return &replication.ReplicationSource{
		Type: &replication.ReplicationSource_Volume{
			Volume: &replication.ReplicationSource_VolumeSource{VolumeId: id},
		},
	}
}

// A single volume's destination is its own handle: the PV keeps it across
// moves and every Replication verb resolves it to the replica. The answer needs
// no relationship yet -- an error would make Ramen refuse the VRG.
func TestReplicationDestinationOfAVolumeIsItsOwnHandle(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	resp, err := cs.GetReplicationDestinationInfo(context.Background(),
		&replication.GetReplicationDestinationInfoRequest{ReplicationSource: volumeSource(testReplVolID)})
	if err != nil {
		t.Fatalf("GetReplicationDestinationInfo: %v", err)
	}
	if got := resp.GetReplicationDestination().GetVolume().GetVolumeId(); got != testReplVolID {
		t.Fatalf("destination volume = %q, want the source handle %q", got, testReplVolID)
	}
	if resp.GetReplicationDestination().GetVolumegroup() != nil {
		t.Fatal("a volume source answered with a group destination")
	}
}

// A group's answer is its own handle and a COMPLETE map over its current
// members, keyed by the members' volume handles exactly as their PVs carry
// them: csi-addons matches persistentVolumeMappingList[].volumeHandle against
// the keys and leaves the destination empty for any miss, which is what made
// Ramen refuse the restore (2026-10-03).
func TestReplicationDestinationOfAGroupMapsEveryMember(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newGroupReplTestServer(t, mock)

	resp, err := cs.GetReplicationDestinationInfo(context.Background(),
		&replication.GetReplicationDestinationInfoRequest{ReplicationSource: groupSource()})
	if err != nil {
		t.Fatalf("GetReplicationDestinationInfo: %v", err)
	}
	vg := resp.GetReplicationDestination().GetVolumegroup()
	if vg == nil {
		t.Fatal("a group source answered without a group destination")
	}
	if vg.GetVolumeGroupId() != vgGroupHandle {
		t.Fatalf("destination group = %q, want %q", vg.GetVolumeGroupId(), vgGroupHandle)
	}
	want := map[string]string{
		sanityClusterID + ":" + sanityPoolUUID + ":" + vgMember1: sanityClusterID + ":" + sanityPoolUUID + ":" + vgMember1,
		sanityClusterID + ":" + sanityPoolUUID + ":" + vgMember2: sanityClusterID + ":" + sanityPoolUUID + ":" + vgMember2,
	}
	got := vg.GetVolumeIds()
	if len(got) != len(want) {
		t.Fatalf("volume_ids = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("volume_ids[%s] = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestReplicationDestinationRejectsAnEmptyOrInvalidSource(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	for name, src := range map[string]*replication.ReplicationSource{
		"empty":      nil,
		"not-handle": volumeSource("not-a-handle"),
		"bad-group": {Type: &replication.ReplicationSource_Volumegroup{
			Volumegroup: &replication.ReplicationSource_VolumeGroupSource{VolumeGroupId: "cg:x:y"},
		}},
	} {
		_, err := cs.GetReplicationDestinationInfo(context.Background(),
			&replication.GetReplicationDestinationInfoRequest{ReplicationSource: src})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code %v (%v), want InvalidArgument", name, status.Code(err), err)
		}
	}
}

// A group the control plane does not know is retryable, never an empty map.
func TestReplicationDestinationOfAnUnknownGroupIsUnavailable(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	_, err := cs.GetReplicationDestinationInfo(context.Background(),
		&replication.GetReplicationDestinationInfoRequest{ReplicationSource: groupSource()})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code %v (%v), want Unavailable", status.Code(err), err)
	}
}
