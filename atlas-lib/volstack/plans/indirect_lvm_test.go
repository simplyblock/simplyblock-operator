// The indirect LVM rows: the VDO shapes with the dmLinear indirection under
// their physical volume, so a namespace that changes subsystem is re-pointed
// below LVM and nothing above it notices (design-cg-live-subsystem-flip.md
// §6.1).

package plans

import (
	"context"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
	"github.com/simplyblock/atlas/volstack/layers"
)

func vdoOptions() LogicalVolumeOptions {
	return LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Deduplication: true, Compression: true},
		PoolName:   "vdopool",
		Capability: volstack.Capability("vdo"),
	}
}

// U-02: the indirection sits between the fabric and the physical volume.
func TestIndirectLVMPutsTheIndirectionUnderThePhysicalVolume(t *testing.T) {
	plan := testNode().IndirectLVM(conn("nqn:vol"), testVolume(), vdoOptions())
	assertShape(t, plan, []string{
		"fabric", "dmLinear", "lvmPhysicalVolume", "lvmVolumeGroup", "lvmLogicalVolume", "filesystem",
	})
}

// U-02: the raw-block VDO row is the indirect LVM row without its filesystem.
func TestIndirectLVMRawBlockIsTheIndirectLVMRowWithoutItsFilesystem(t *testing.T) {
	plan := testNode().IndirectLVMRawBlock(conn("nqn:vol"), testVolume(), vdoOptions())
	assertShape(t, plan, []string{
		"fabric", "dmLinear", "lvmPhysicalVolume", "lvmVolumeGroup", "lvmLogicalVolume",
	})
}

// U-03: the indirection changes where the physical volume sits and nothing
// about what the logical volume is.
func TestIndirectLVMCarriesTheVDOParametersUnchanged(t *testing.T) {
	plan := testNode().IndirectLVM(conn("nqn:vol"), testVolume(), vdoOptions())
	params, ok := plan[4].(volstack.Recorder).Params().(layers.LVMLogicalVolumeParams)
	if !ok {
		t.Fatalf("the logical-volume layer carries %T", plan[4].(volstack.Recorder).Params())
	}
	if params.PoolName != "vdopool" || !params.Deduplication || !params.Compression {
		t.Errorf("vdo parameters: got %+v", params)
	}
	if req, ok := plan[4].(volstack.NodeRequirements); !ok || req.NodeCapability() != "vdo" {
		t.Errorf("the logical-volume layer lost its node requirement")
	}
}

// U-04: LVM must only ever see the mapping. The namespace device carries the
// same physical-volume label, and an LVM command scoped to it would find a
// duplicate, or worse, write through a path the swap is about to remove.
func TestIndirectLVMScopesLVMToTheMappingNeverTheNamespace(t *testing.T) {
	commands := &recordingLVM{out: map[string]string{}}
	node := NewNode(NodeConfig{
		Manager: lvm.NewManagerWithRunner(commands.run),
		Content: blankDevice{},
	})
	plan := node.IndirectLVM(conn("nqn:vol"), testVolume(), vdoOptions())

	mapping := volstack.Artifact{Devices: []blockdev.Device{{Path: "/dev/mapper/sb-3f2b1c"}}}
	if _, err := plan[2].Ensure(context.Background(), mapping); err != nil {
		t.Fatalf("ensure the physical volume over the mapping: %v", err)
	}

	scoped := false
	for _, call := range commands.calls {
		line := strings.Join(call, " ")
		if strings.Contains(line, "/dev/nvme") {
			t.Errorf("an LVM command named the namespace device: %s", line)
		}
		if strings.Contains(line, "/dev/mapper/sb-3f2b1c") {
			scoped = true
		}
	}
	if !scoped {
		t.Fatalf("no LVM command named the mapping:\n%v", commands.calls)
	}
}
