// Tests for staging a ReadOnlyMany raw-block volume read-only.

package node

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/volstack/plans"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func blockCapabilityIn(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

// Regression: 2026-11-25-rox-block-writable. A ReadOnlyMany raw-block claim
// attached on two nodes and both pods wrote to it: the device handed up was
// the writable dm-linear mapping every raw-block volume gets.
func TestAReadOnlyManyBlockVolumeIsStagedReadOnly(t *testing.T) {
	for mode, want := range map[csi.VolumeCapability_AccessMode_Mode]bool{
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:  true,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER: false,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:      false,
	} {
		if got := stackVolume("/staging", map[string]string{}, blockCapabilityIn(mode)).ReadOnly; got != want {
			t.Errorf("%s: ReadOnly = %v, want %v", mode, got, want)
		}
	}
}

// Read-only is enforced by the dm-linear mapping, so a plan without one cannot
// stage the volume at all: a writable device would be the silent alternative.
func TestReadOnlyNeedsTheIndirection(t *testing.T) {
	readOnly := plans.Volume{ReadOnly: true}
	if err := refuseUnenforcedReadOnly(readOnly, shapeIndirectRawBlock); err != nil {
		t.Errorf("indirect raw block: %v, want it staged", err)
	}
	if err := refuseUnenforcedReadOnly(readOnly, shapeRawBlock); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("raw block without the indirection: %v, want FailedPrecondition", err)
	}
	if err := refuseUnenforcedReadOnly(plans.Volume{}, shapeRawBlock); err != nil {
		t.Errorf("a writable volume without the indirection: %v, want it staged", err)
	}
}
