// What the physical-volume layer owes a plan that hands it several devices.
//
// A striped volume's plan is members(n) -> lvmPV -> lvmVolume, so this layer is
// handed every namespace at once and the volume group above it is created over
// all of them. Every other test in this package hands it one, which is why the
// gap survived until the suite ran the LVM plans on a node.

package layers

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// belowMembers is what a fan-in layer exposes: several devices, in order.
func belowMembers(n int) volstack.Artifact {
	devices := make([]blockdev.Device, 0, n)
	for i := range n {
		name := fmt.Sprintf("nvme%dn1", i)
		devices = append(devices, blockdev.Device{
			Path: "/dev/" + name, Name: name, LogicalBlockSize: 512, SizeBytes: 1 << 30,
		})
	}
	return volstack.Artifact{Devices: devices}
}

// issuedFor is the device each call to this command named, in order.
func issuedFor(cmds *lvmCommands, command string) []string {
	var devices []string
	for _, call := range cmds.calls {
		if call[0] == command {
			devices = append(devices, call[len(call)-1])
		}
	}
	return devices
}

// Regression: 2026-09-05-lvmpv-labelled-one-device-of-many. The layer asked the
// artifact below for a single device, which no fan-in layer exposes, so a
// striped plan failed before labeling anything, reporting that the layer below
// exposed no single device to label.
//
// Found by the integration suite the first time the LVM plans ran against a real
// kernel, on a plan the unit tests had never built.
func TestLVMPVLabelsEveryDeviceBelow(t *testing.T) {
	cmds := newLVM()
	l := newLVMPV(cmds, blockdev.Reading{Content: blockdev.ContentBlank}, nil)
	below := belowMembers(3)

	state, _, err := l.Observe(context.Background(), below)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateAbsent {
		t.Fatalf("state = %s, want Absent", state)
	}

	own, err := l.Ensure(context.Background(), below)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(own.Devices) != 3 {
		t.Errorf("exposed %d devices, want the three it was handed", len(own.Devices))
	}

	want := []string{"/dev/nvme0n1", "/dev/nvme1n1", "/dev/nvme2n1"}
	if got := issuedFor(cmds, "pvcreate"); !slices.Equal(got, want) {
		t.Errorf("labeled %v, want %v", got, want)
	}
}

// A teardown unlabels every device it labeled, and in the order it was handed
// them, for the same reason a bring-up labels every one.
func TestLVMPVUnlabelsEveryDeviceBelow(t *testing.T) {
	cmds := newLVM()
	l := newLVMPV(cmds, lvmLabel(), nil)

	if err := l.Destroy(context.Background(), belowMembers(2)); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	want := []string{"/dev/nvme0n1", "/dev/nvme1n1"}
	if got := issuedFor(cmds, "pvremove"); !slices.Equal(got, want) {
		t.Errorf("unlabeled %v, want %v", got, want)
	}
}

// A device that cannot be labeled stops the layer, and the ones already labeled
// stay labeled: a half-labeled set is what Observe reports as partial, and the
// next bring-up completes it rather than starting over.
func TestLVMPVStopsAtTheDeviceItCannotLabel(t *testing.T) {
	cmds := newLVM()
	l := newLVMPV(cmds, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)

	if _, err := l.Ensure(context.Background(), belowMembers(2)); err == nil {
		t.Fatal("Ensure labeled a set holding a device carrying a filesystem")
	}
	if got := issuedFor(cmds, "pvcreate"); len(got) != 0 {
		t.Errorf("it labeled %v anyway", got)
	}
}
