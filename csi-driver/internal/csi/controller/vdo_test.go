// CreateVolume's refusals for a client-side compression or deduplication
// class, which the node stages as LVM and VDO on the host that attaches it.

package controller

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// vdoRequest asks for a raw-block volume of a class that sets param to true,
// in the given access mode.
func vdoRequest(param string, mode csi.VolumeCapability_AccessMode_Mode) *csi.CreateVolumeRequest {
	req := basicCreateVolumeRequest("pvc-vdo")
	req.Parameters[param] = "true"
	req.VolumeCapabilities = []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}}
	return req
}

// Regression: 2026-11-25-vdo-multi-node. A client-side compression or
// deduplication volume is an LVM volume group holding a VDO volume, which the
// node activates on the host that stages it. Advertising ReadWriteMany for the
// pNFS work let a raw-block ReadWriteMany claim of such a class bind, and a
// live-migrated VM's disk then had two hosts activate one group and run VDO
// over the same blocks, with nothing coordinating either between them. Every
// multi-node mode is refused before anything is created.
func TestAVDOVolumeRefusesMultiNodeAccess(t *testing.T) {
	for _, param := range []string{"client_compression", "client_deduplication"} {
		for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		} {
			t.Run(param+"/"+mode.String(), func(t *testing.T) {
				mock := newMockSBCLI()
				defer mock.Close()
				cs := newTestControllerServer(t, mock)

				_, err := cs.CreateVolume(context.Background(), vdoRequest(param, mode))
				if status.Code(err) != codes.InvalidArgument {
					t.Errorf("CreateVolume = %v, want InvalidArgument", err)
				}
				if n := len(volumesNamed(mock)); n != 0 {
					t.Errorf("a refused create made %d volumes", n)
				}
			})
		}
	}
}

// The refusal is about the access mode, not the class: a single-node raw-block
// claim of the same class is created as before.
func TestAVDOVolumeOnOneNodeIsCreated(t *testing.T) {
	for _, param := range []string{"client_compression", "client_deduplication"} {
		t.Run(param, func(t *testing.T) {
			mock := newMockSBCLI()
			defer mock.Close()
			cs := newTestControllerServer(t, mock)

			req := vdoRequest(param, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
			if _, err := cs.CreateVolume(context.Background(), req); err != nil {
				t.Fatalf("CreateVolume: %v", err)
			}
			if n := len(volumesNamed(mock)); n != 1 {
				t.Errorf("CreateVolume made %d volumes, want 1", n)
			}
		})
	}
}
