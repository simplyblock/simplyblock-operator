// Tests for which ReadOnlyMany claims CreateVolume accepts.

package controller

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func capability(block bool, mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	c := &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode}}
	if block {
		c.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
	} else {
		c.AccessType = &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}
	}
	return c
}

const (
	readerOnly = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	oneWriter  = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
)

func TestAReadOnlyManyBlockVolumeIsCreated(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	req := basicCreateVolumeRequest("pvc-rox")
	req.VolumeCapabilities = []*csi.VolumeCapability{capability(true, readerOnly)}

	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if n := len(volumesNamed(mock)); n != 1 {
		t.Errorf("CreateVolume made %d volumes, want 1", n)
	}
}

// Regression: 2026-11-25-rox-filesystem. A ReadOnlyMany filesystem claim of a
// block class bound, and its pods never started: the node mounted the
// filesystem read-only, and CRI-O could not apply the SELinux relabel to it.
// Several hosts mounting one block filesystem is also not something this
// driver coordinates. It is refused before anything is created.
func TestAReadOnlyManyFilesystemOnABlockClassIsRefused(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	req := basicCreateVolumeRequest("pvc-rox-fs")
	req.VolumeCapabilities = []*csi.VolumeCapability{capability(false, readerOnly)}

	if _, err := cs.CreateVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateVolume = %v, want InvalidArgument", err)
	}
	if n := len(volumesNamed(mock)); n != 0 {
		t.Errorf("a refused create made %d volumes", n)
	}
}

// Kubernetes stages a volume whose claim lists ReadOnlyMany among its modes as
// ReadOnlyMany, so a writer among its consumers would be handed a read-only
// device. The mixture is refused rather than left to fail at the writer.
func TestReadOnlyManyMixedWithAWriterIsRefused(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	req := basicCreateVolumeRequest("pvc-rox-rwo")
	req.VolumeCapabilities = []*csi.VolumeCapability{capability(true, readerOnly), capability(true, oneWriter)}

	if _, err := cs.CreateVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateVolume = %v, want InvalidArgument", err)
	}
	if n := len(volumesNamed(mock)); n != 0 {
		t.Errorf("a refused create made %d volumes", n)
	}
}

// With ReadWriteOncePod advertised, Kubernetes sends SINGLE_NODE_SINGLE_WRITER
// for an RWOP claim and SINGLE_NODE_MULTI_WRITER for every RWO one. Both are
// created, in either volume mode.
func TestTheReadWriteOncePodModesAreCreated(t *testing.T) {
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	} {
		for _, block := range []bool{true, false} {
			mock := newMockSBCLI()
			cs := newTestControllerServer(t, mock)
			req := basicCreateVolumeRequest("pvc-rwop")
			req.VolumeCapabilities = []*csi.VolumeCapability{capability(block, mode)}
			if _, err := cs.CreateVolume(context.Background(), req); err != nil {
				t.Errorf("%s (block %v): CreateVolume = %v, want it created", mode, block, err)
			}
			mock.Close()
		}
	}
}

// Regression: #989 review (ValidateVolumeCapabilities). Advertising
// ReadOnlyMany made ValidateVolumeCapabilities confirm it for a filesystem,
// because the check looked only at the advertised modes. A static or
// pre-existing volume never passes CreateVolume's refusal, so the confirmation
// was what let a ReadOnlyMany filesystem reach a node that cannot stage one.
// A raw-block ReadOnlyMany capability stays confirmed.
func TestValidateVolumeCapabilitiesAppliesTheReadOnlyManyRules(t *testing.T) {
	mock := newMockSBCLI()
	defer mock.Close()
	cs := newTestControllerServer(t, mock)
	cs.Driver.AddVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
	})
	ctx := context.Background()
	created, err := cs.CreateVolume(ctx, basicCreateVolumeRequest("pvc-validate-rox"))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	validate := func(c *csi.VolumeCapability) *csi.ValidateVolumeCapabilitiesResponse {
		t.Helper()
		resp, err := cs.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
			VolumeId:           created.GetVolume().GetVolumeId(),
			VolumeCapabilities: []*csi.VolumeCapability{c},
		})
		if err != nil {
			t.Fatalf("ValidateVolumeCapabilities: %v", err)
		}
		return resp
	}
	rox := &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY}

	filesystem := validate(&csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: rox})
	if filesystem.GetConfirmed() != nil {
		t.Error("a ReadOnlyMany filesystem was confirmed, which no node can stage")
	}
	if filesystem.GetMessage() == "" {
		t.Error("the refusal carries no message saying why")
	}
	block := validate(&csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}, AccessMode: rox})
	if block.GetConfirmed() == nil {
		t.Errorf("ReadOnlyMany raw block was not confirmed: %q", block.GetMessage())
	}
}
