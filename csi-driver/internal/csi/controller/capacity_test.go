// The capacity a volume under an LVM stack reports against the device the node
// builds for it. LVM keeps lvm.PhysicalVolumeOverheadBytes of each control-plane
// volume it stacks over, so a volume sized at what it reports would build a
// device short of it, and a raw-block consumer filling the reported size would
// write past its end.
package controller

import (
	"context"
	"strconv"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/simplyblock/atlas/lvm"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// deviceBytes is the device LVM builds over members of these sizes: their sum,
// less what LVM keeps of each.
func deviceBytes(sizes []int64) int64 {
	var sum int64
	for _, s := range sizes {
		sum += s - lvm.PhysicalVolumeOverheadBytes
	}
	return sum
}

func memberSizes(mock *mockSBCLI) []int64 {
	volumes := volumesNamed(mock)
	sizes := make([]int64, 0, len(volumes))
	for _, v := range volumes {
		sizes = append(sizes, v.Size)
	}
	return sizes
}

// Regression: 2026-11-17-lvm-capacity-overreported (PR #922 review). A striped
// volume reported its members' sum, and the striped device LVM built over them
// was lvm.PhysicalVolumeOverheadBytes short per member: 10Gi across two members
// was reported as 10Gi on a device 8MiB smaller. Whatever the request and the
// width, the reported capacity is at least the request and at most the device.
func TestAStripedVolumeReportsNoMoreThanItsDevice(t *testing.T) {
	for _, tc := range []struct {
		width    string
		required int64
	}{
		{"2", 10 * gib},
		{"3", 10 * gib},
		{"2", 1 * gib},
		{"4", 7*gib + 512*mib},
	} {
		mock := newMockSBCLI()
		cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC, stripeNodeD}))
		vol := createStriped(t, cs, stripedRequest("pvc-striped", tc.width, tc.required))

		device := deviceBytes(memberSizes(mock))
		if got := vol.GetCapacityBytes(); got < tc.required || got > device {
			t.Errorf("width %s, %d bytes asked: reported %d on a device of %d", tc.width, tc.required, got, device)
		}
		mock.Close()
	}
}

// The same holds after an expansion, which resizes every member.
func TestAnExpandedStripedVolumeReportsNoMoreThanItsDevice(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "2", 4*gib))

	resp, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      vol.GetVolumeId(),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 10 * gib},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	device := deviceBytes(memberSizes(mock))
	if got := resp.GetCapacityBytes(); got < 10*gib || got > device {
		t.Errorf("expanded to 10GiB: reported %d on a device of %d", got, device)
	}
}

// Regression: 2026-11-17-lvm-capacity-overreported, for the stack that was
// there first. A client-side compressed or deduplicated volume is one LVM
// physical volume, created at exactly the size it reports.
func TestAVDOVolumeReportsNoMoreThanItsLVMDevice(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	req := basicCreateVolumeRequest("pvc-vdo")
	req.CapacityRange.RequiredBytes = 10 * gib
	req.Parameters["client_compression"] = strconv.FormatBool(true)

	resp, err := cs.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	device := deviceBytes(memberSizes(mock))
	if got := resp.GetVolume().GetCapacityBytes(); got < 10*gib || got > device {
		t.Errorf("reported %d on an LVM device of %d", got, device)
	}
}

// An expansion of a client-side compressed volume, which receives no volume
// context, reads its PersistentVolume and grows it as it was created.
func TestAnExpandedVDOVolumeReportsNoMoreThanItsLVMDevice(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	req := basicCreateVolumeRequest("pvc-vdo")
	req.CapacityRange.RequiredBytes = 6 * gib
	req.Parameters["client_compression"] = strconv.FormatBool(true)
	created, err := cs.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	cs.kubeClient = k8sfake.NewClientset(&corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-vdo"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{
				VolumeHandle:     created.GetVolume().GetVolumeId(),
				VolumeAttributes: created.GetVolume().GetVolumeContext(),
			},
		}},
	})

	resp, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      created.GetVolume().GetVolumeId(),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 10 * gib},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	device := deviceBytes(memberSizes(mock))
	if got := resp.GetCapacityBytes(); got < 10*gib || got > device {
		t.Errorf("expanded to 10GiB: reported %d on an LVM device of %d", got, device)
	}
}

// Regression: 2026-11-17-striped-restore-size (PR #922 review). A striped
// snapshot reported the sum of its members as its restore size, which is more
// than the source volume reports whenever the members are larger than an equal
// share: 10Gi across three members of 4GiB listed as 12Gi, and the external
// provisioner refuses a restore into a claim smaller than that, so the claim
// the volume was made from could not be restored into. A snapshot reports no
// more than its source, as created and as listed, and a restore at that size
// gets members as large as the snapshot's.
func TestAStripedSnapshotRestoresIntoItsSourcesSize(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := stripedServer(t, mock, storageNodes([]string{stripeNodeA, stripeNodeB, stripeNodeC}))
	vol := createStriped(t, cs, stripedRequest("pvc-striped", "3", 10*gib))

	snap := snapshotStriped(t, cs, vol.GetVolumeId(), "snapshot-1")
	if snap.GetSizeBytes() > vol.GetCapacityBytes() {
		t.Errorf("the snapshot's restore size is %d, more than its source's %d", snap.GetSizeBytes(), vol.GetCapacityBytes())
	}
	list, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: snap.GetSnapshotId()})
	if err != nil || len(list.GetEntries()) != 1 {
		t.Fatalf("ListSnapshots: %v, %v", list, err)
	}
	if listed := list.GetEntries()[0].GetSnapshot().GetSizeBytes(); listed != snap.GetSizeBytes() {
		t.Errorf("the snapshot lists at %d and was created at %d", listed, snap.GetSizeBytes())
	}
	if stripeMemberBytes(snap.GetSizeBytes(), 3) < memberSizes(mock)[0] {
		t.Errorf("a restore at the snapshot's size (%d) would get members of %d, smaller than the snapshot's %d",
			snap.GetSizeBytes(), stripeMemberBytes(snap.GetSizeBytes(), 3), memberSizes(mock)[0])
	}
}

// Regression: 2026-11-17-vdo-capacity-underreported (PR #922 review). VDO keeps
// its own metadata on top of LVM's extent, 4.02GiB of a 12GiB pool on the
// runner kernel, and the volume reported the whole. A VDO volume's pool holds
// what it reports plus both, created and expanded, and its context names the
// logical size the node creates the VDO volume at.
func TestAVDOVolumesPoolHoldsItsReportedSizeAndVDOsMetadata(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	req := basicCreateVolumeRequest("pvc-vdo")
	req.CapacityRange.RequiredBytes = 10 * gib
	req.Parameters["client_compression"] = strconv.FormatBool(true)

	resp, err := cs.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	reported := resp.GetVolume().GetCapacityBytes()
	need := reported + lvm.PhysicalVolumeOverheadBytes + lvm.VDOOverheadBytes(reported)
	if got := memberSizes(mock)[0]; got < need {
		t.Errorf("the pool's volume is %d bytes, short of the %d a %d-byte VDO volume needs", got, need, reported)
	}
	if got := resp.GetVolume().GetVolumeContext()[csicommon.CtxVDOLogicalBytes]; got != strconv.FormatInt(reported, 10) {
		t.Errorf("the context names a logical size of %q, want %d", got, reported)
	}
}
