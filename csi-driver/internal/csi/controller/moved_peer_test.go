// Tests for controller calls on a volume of another site's control plane (a
// split control plane) after it failed over onto this site: the PV keeps the
// handle of the source volume on the other site, which may be cut off, and
// expand, snapshot, and clone reach the clone here through the relationship
// this site's control plane recorded, without asking the other site first.

package controller

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// failedOverVolume is this site after the fail-over of movedSourceID: the
// clone movedTargetID here, and the relationship that leads to it.
func failedOverVolume(m *mockSBCLI) {
	m.volumes[movedTargetID] = &mockVolume{UUID: movedTargetID, Name: "clone", Size: 1 << 30}
	rel := moved(movedSourceID, movedTargetID, movedTargetID)
	rel["state"], rel["mode"], rel["target_cluster_id"] = "failed_over", "replication", siteBClusterID
	m.replicationRelationship[movedSourceID] = rel
}

func TestCallsOnAPeerVolumeFailedOverHereReachTheClone(t *testing.T) {
	ctx := context.Background()

	t.Run("expand", func(t *testing.T) {
		local := newMockSBCLI()
		defer local.Close()
		cs := splitSite(t, local, true)
		failedOverVolume(local)

		_, err := cs.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
			VolumeId: movedHandle, CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
		})

		if err != nil {
			t.Fatalf("ControllerExpandVolume: %v (asked the cut-off source's control plane?)", err)
		}
		if got := local.volumes[movedTargetID].Size; got != 4<<30 {
			t.Errorf("clone size = %d, want it grown to 4 GiB", got)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		local := newMockSBCLI()
		defer local.Close()
		cs := splitSite(t, local, true)
		failedOverVolume(local)

		_, err := cs.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{SourceVolumeId: movedHandle, Name: "snap-1"})

		if err != nil {
			t.Fatalf("CreateSnapshot: %v (asked the cut-off source's control plane?)", err)
		}
		var of string
		for _, s := range local.snapshots {
			of = s.VolUUID
		}
		if of != movedTargetID {
			t.Errorf("the snapshot was taken of %q, want the clone", of)
		}
	})

	t.Run("clone", func(t *testing.T) {
		local := newMockSBCLI()
		defer local.Close()
		cs := splitSite(t, local, true)
		failedOverVolume(local)
		req := basicCreateVolumeRequest("pvc-clone")
		req.Parameters["cluster_id"] = siteBClusterID // this site's storage class
		req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: movedHandle},
		}}

		if _, err := cs.CreateVolume(ctx, req); err != nil {
			t.Fatalf("CreateVolume: %v (asked the cut-off source's control plane?)", err)
		}
		if local.lastCloneSource != movedTargetID {
			t.Errorf("cloned from %q, want the clone", local.lastCloneSource)
		}
	})
}

func TestAPeerVolumeNotFailedOverHereIsLeftToItsControlPlane(t *testing.T) {
	// No fail-over here: the volume's own control plane serves it. With that
	// one cut off, the expand fails rather than acting on something here.
	local := newMockSBCLI()
	defer local.Close()
	cs := splitSite(t, local, true)

	_, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: movedHandle, CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	})

	if err == nil {
		t.Fatal("ControllerExpandVolume succeeded for a volume nothing here serves")
	}
}
