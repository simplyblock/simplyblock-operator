package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// TestControllerGetVolumeEchoesTheRequestedID pins what the CSI spec requires
// of ControllerGetVolume: the volume it returns is identified by the volume_id
// it was asked about, which is the handle, not the lvol UUID inside it.
//
// The identifier used to change shape between the two paths, the full handle
// when the lookup failed and the bare lvol UUID when it succeeded, so a caller
// correlating the response against its own request matched on failure and not
// on success.
func TestControllerGetVolumeEchoesTheRequestedID(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	created, err := cs.CreateVolume(context.Background(), basicCreateVolumeRequest("echo-id"))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	handle := created.GetVolume().GetVolumeId()

	got, err := cs.ControllerGetVolume(context.Background(), &csi.ControllerGetVolumeRequest{VolumeId: handle})
	if err != nil {
		t.Fatalf("ControllerGetVolume: %v", err)
	}
	if id := got.GetVolume().GetVolumeId(); id != handle {
		t.Errorf("ControllerGetVolume returned volume id %q, want the handle it was asked about, %q", id, handle)
	}
}

// TestControllerGetVolumeEchoesTheRequestedIDWhenAbnormal pins the same thing
// on the path where the volume cannot be read, so that the two agree.
func TestControllerGetVolumeEchoesTheRequestedIDWhenAbnormal(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)

	handle := fmt.Sprintf("%s:%s:%s", sanityClusterID, sanityPoolUUID, "22222222-2222-2222-2222-222222222222")

	got, err := cs.ControllerGetVolume(context.Background(), &csi.ControllerGetVolumeRequest{VolumeId: handle})
	if err != nil {
		t.Fatalf("ControllerGetVolume: %v", err)
	}
	if id := got.GetVolume().GetVolumeId(); id != handle {
		t.Errorf("ControllerGetVolume returned volume id %q, want %q", id, handle)
	}
}
