// Tests for staging a ReadOnlyMany raw-block volume read-only.

package node

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/volstack/plans"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
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

// Regression: #989 review (striped ReadOnlyMany). attachPlan returned a
// striped volume's plan before the read-only guard, and a striped volume never
// takes the dm-linear indirection, so a static or pre-existing striped volume
// claimed ReadOnlyMany staged a writable device. CreateVolume's refusal does
// not cover a volume it did not create.
func TestAStripedReadOnlyManyBlockVolumeIsRefused(t *testing.T) {
	ns := &Server{}
	vc := map[string]string{csicommon.CtxStripeWidth: "3"}
	_, err := ns.attachPlan(context.Background(), "vol", "/staging", vc,
		blockCapabilityIn(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("attachPlan = %v, want FailedPrecondition before anything attaches", err)
	}
}

// Regression: #989 review (node admission). A ReadOnlyMany filesystem on a
// block volume reached the node whenever CreateVolume had not seen it (a static
// or pre-existing volume), and the node staged it: the read-only mount CRI-O
// then cannot relabel. The node refuses it before it attaches anything.
func TestAReadOnlyManyFilesystemIsRefusedAtStage(t *testing.T) {
	runner := newRecordingRunner()
	ns, _ := newStackedServer(t, runner)
	req := stageRequest(t)
	req.VolumeCapability.AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY

	_, err := ns.NodeStageVolume(context.Background(), req)

	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("NodeStageVolume = %v, want InvalidArgument for a ReadOnlyMany filesystem", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("the stage ran %v before refusing", runner.calls)
	}
}
