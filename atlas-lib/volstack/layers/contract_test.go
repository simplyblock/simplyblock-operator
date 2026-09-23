// The one rule every layer in this package has to obey, asserted over all of
// them at once rather than in each one's own file.
//
// A teardown walks a stack whose foundation is usually already gone: releasing
// detaches the fabric, so by the time the walk reaches anything above it, the
// device those layers stood on has been taken away. The runner decides what to
// skip from the state each layer reports, and it can only do that if the layer
// reports a state. A layer that returns an error there stops the walk instead,
// and the volume it belonged to is never released.
//
// That is not a rule the runner can apply on every layer's behalf, because what
// a missing device means is genuinely the layer's own answer: an LVM volume
// group with no member device left may still be mapped as live device-mapper
// nodes this host has to release, while a physical-volume label cannot outlive
// the device it was written on. So each layer answers, and this makes sure each
// one answers at all — including the next one somebody writes, which is the
// case a test in any single layer's file would not cover.

package layers

import (
	"context"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/volstack"
)

// shippedLayers is every layer this package provides, built with seams that
// answer rather than reach a host. A layer added here without a row is a layer
// the rule below has never been checked against.
func shippedLayers(t *testing.T) map[string]volstack.Layer {
	t.Helper()

	commands := newLVM()
	fs := newFakeFS()

	return map[string]volstack.Layer{
		"fabric": NewFabric(FabricConfig{
			Connection: lvol.Connection{NQN: "nqn.2023-02.io.simplyblock:vol", NSID: 1},
			Connector:  &fakeConnector{},
			Devices:    &fakeDevices{},
		}),
		"members": NewMembers(volstack.Plan{}),
		"lvmPhysicalVolume": NewLVMPhysicalVolume(LVMPhysicalVolumeConfig{
			VolumeGroup:   "vol-x",
			LogicalVolume: "lv-x",
			Manager:       lvm.NewManagerWithRunner(commands.run),
			Content:       fakeReader{reading: blockdev.Reading{Content: blockdev.ContentBlank}},
		}),
		"lvmVolumeGroup": NewLVMVolumeGroup(LVMVolumeGroupConfig{
			VolumeGroup: "vol-x",
			Manager:     lvm.NewManagerWithRunner(commands.run),
		}),
		"lvmLogicalVolume": NewLVMLogicalVolume(LVMLogicalVolumeConfig{
			VolumeGroup:   "vol-x",
			LogicalVolume: "lv-x",
			Manager:       lvm.NewManagerWithRunner(commands.run),
		}),
		"filesystem": NewFilesystem(FilesystemConfig{
			FsType:      "ext4",
			StagingPath: stagingPath,
			Ops:         fs,
			Content:     fakeReader{reading: blockdev.Reading{Content: blockdev.ContentBlank}},
		}),
	}
}

// Observe answers with a state when the layer below exposes nothing, rather than
// with an error.
//
// Which state is each layer's own business, and this asserts none of it. What it
// asserts is that the runner gets an answer it can act on, because a teardown
// reaching a layer that errors here stops there and strands the volume.
func TestEveryLayerObservesADeadFoundationAsAState(t *testing.T) {
	for name, layer := range shippedLayers(t) {
		t.Run(name, func(t *testing.T) {
			state, _, err := layer.Observe(context.Background(), volstack.Artifact{})
			if err != nil {
				t.Fatalf("Observe with nothing below returned an error rather than a state: %v\n"+
					"a teardown reaching this layer stops here, and the volume is never released", err)
			}
			t.Logf("answers %s, which is this layer's own call", state)
		})
	}
}

// Release succeeds against a layer that has nothing below it, for the same
// reason: it is the ordinary state on the teardown path, since the release of
// the layer underneath is what took the device away.
func TestEveryLayerReleasesWithADeadFoundation(t *testing.T) {
	for name, layer := range shippedLayers(t) {
		t.Run(name, func(t *testing.T) {
			if err := layer.Release(context.Background(), volstack.Artifact{}); err != nil {
				t.Errorf("Release with nothing below: %v", err)
			}
		})
	}
}

// Destroy is the same rule once more. The runner skips a layer its survey found
// absent, so this covers the layer that reported itself present on host state
// alone — a volume group still mapped with no member device left to read.
func TestEveryLayerDestroysWithADeadFoundation(t *testing.T) {
	for name, layer := range shippedLayers(t) {
		t.Run(name, func(t *testing.T) {
			if err := layer.Destroy(context.Background(), volstack.Artifact{}); err != nil {
				t.Errorf("Destroy with nothing below: %v", err)
			}
		})
	}
}
