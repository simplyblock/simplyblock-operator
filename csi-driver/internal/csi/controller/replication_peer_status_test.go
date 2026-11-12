// Tests for the status verbs of a group on a split control plane after a
// fail-over with its source cut off: the VolumeGroupReplication on this site
// keeps the source's group handle, and this site's own control plane, not the
// source's, answers where the group lives now.
package controller

import (
	"context"
	"testing"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// failedOverHere is the group a fail-over made on this site, and its clone of
// vgMember1.
const (
	failedOverHere = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	cloneHere      = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
)

// failedOverOnThisSite is site B after the fail-over of the source's group:
// the clones' group here, replicated last at lastSync, and this site's control
// plane resolving the source's group handle to it.
func failedOverOnThisSite(t *testing.T, lastSync int64) (*Server, *mockSBCLI) {
	t.Helper()
	local := newMockSBCLI()
	t.Cleanup(local.Close)
	local.seedGroup(failedOverHere, cloneHere)
	local.groups[failedOverHere].LastReplicatedAt = lastSync
	local.replicatedGroupResolution[sanityClusterID+"/"+vgGroupID] = map[string]any{
		"cluster_id": sanityClusterID, "group_id": vgGroupID,
		"active_cluster_id": siteBClusterID, "active_group_id": failedOverHere,
		"members": []any{map[string]any{
			"origin_handle": sanityClusterID + ":pool:" + vgMember1,
			"active_handle": siteBClusterID + ":pool:" + cloneHere,
		}},
	}
	return splitSite(t, local, true), local
}

func TestInfoOfAGroupFailedOverHereComesFromThisSite(t *testing.T) {
	cs, _ := failedOverOnThisSite(t, 1794490000)

	resp, err := cs.GetVolumeReplicationInfo(context.Background(),
		&replication.GetVolumeReplicationInfoRequest{ReplicationSource: groupSource()})
	if err != nil {
		t.Fatalf("GetVolumeReplicationInfo: %v (asked the cut-off source's control plane?)", err)
	}
	if got := resp.GetLastSyncTime().GetSeconds(); got != 1794490000 {
		t.Errorf("last sync = %d, want the failed-over group's 1794490000", got)
	}
}

func TestDestinationInfoOfAGroupFailedOverHereComesFromThisSite(t *testing.T) {
	cs, _ := failedOverOnThisSite(t, 1794490000)

	resp, err := cs.GetReplicationDestinationInfo(context.Background(),
		&replication.GetReplicationDestinationInfoRequest{ReplicationSource: groupSource()})
	if err != nil {
		t.Fatalf("GetReplicationDestinationInfo: %v (asked the cut-off source's control plane?)", err)
	}
	vg := resp.GetReplicationDestination().GetVolumegroup()
	want := sanityClusterID + ":pool:" + vgMember1
	if vg.GetVolumeGroupId() != vgGroupHandle || len(vg.GetVolumeIds()) != 1 || vg.GetVolumeIds()[want] != want {
		t.Errorf("destination = %v, want group %s mapping %s to itself", vg, vgGroupHandle, want)
	}
}

func TestInfoOfAGroupNotFailedOverHereAsksItsOwnControlPlane(t *testing.T) {
	// Before a fail-over, the source's control plane serves the group: with
	// it cut off, the status is unavailable, as before.
	local := newMockSBCLI()
	t.Cleanup(local.Close)
	cs := splitSite(t, local, true)

	_, err := cs.GetVolumeReplicationInfo(context.Background(),
		&replication.GetVolumeReplicationInfoRequest{ReplicationSource: groupSource()})
	if err == nil {
		t.Fatal("GetVolumeReplicationInfo answered for a group nothing here serves")
	}
	if c := status.Code(err); c == codes.OK || c == codes.InvalidArgument {
		t.Errorf("error = %v, want a retryable failure from the source's control plane", err)
	}
}
