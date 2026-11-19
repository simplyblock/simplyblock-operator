// What the physical-volume layer owes a plan that hands it several devices.
//
// A striped volume's plan is members(n) -> lvmPhysicalVolume -> lvmVolumeGroup,
// so this layer is
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

// readingByDevice answers each device with its own reading, for a set whose
// members do not all carry the same thing.
type readingByDevice map[string]blockdev.Reading

func (r readingByDevice) Read(_ context.Context, dev blockdev.Device) (blockdev.Reading, error) {
	return r[dev.Path], nil
}

// importsOf is every vgimportclone the layer issued.
func importsOf(cmds *lvmCommands) [][]string {
	var imports [][]string
	for _, call := range cmds.calls {
		if call[0] == "vgimportclone" {
			imports = append(imports, call)
		}
	}
	return imports
}

// sourceVG is the group a clone's members carry until they are re-stamped.
const sourceVG = "vol-44444444-4444-4444-4444-444444444444"

// Regression: 2026-11-17-striped-clone-vgimportclone (PR #922 review). A clone
// of a striped volume carries its source's group on every member, and the group
// spans them. The layer re-identified the members one device at a time, a
// vgimportclone scoped to that device alone, which LVM refuses for a group
// whose other devices it was told not to look at ("Cannot change VG while PVs
// are missing"), so no restore or clone of a striped volume could be staged.
// The set is one group and is imported as one, in member order.
func TestLVMPVReidentifiesAClonedSetInOneImport(t *testing.T) {
	cmds := newLVM()
	cmds.byDevice = map[string]string{"/dev/nvme0n1": sourceVG, "/dev/nvme1n1": sourceVG}
	l := newLVMPV(cmds, lvmLabel(), nil)
	below := belowMembers(2)

	state, _, err := l.Observe(context.Background(), below)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateForeign {
		t.Fatalf("state = %s, want Foreign", state)
	}
	if _, err := l.Ensure(context.Background(), below); err != nil {
		t.Fatalf("Ensure: %v\n%s", err, cmds.issued())
	}

	imports := importsOf(cmds)
	if len(imports) != 1 {
		t.Fatalf("issued %d imports, want one over the whole set:\n%s", len(imports), cmds.issued())
	}
	members := imports[0][len(imports[0])-2:]
	if want := []string{"/dev/nvme0n1", "/dev/nvme1n1"}; !slices.Equal(members, want) {
		t.Errorf("imported %v, want every member in order %v:\n%s", members, want, cmds.issued())
	}
	if !slices.Contains(imports[0], testVG) {
		t.Errorf("the import does not name the volume's own group %s: %v", testVG, imports[0])
	}
}

// A set that is partly this volume's group and partly another's is what an
// import that died between two members leaves. Importing the stale member on
// its own would make it a second group, and the stripe would come up with a
// hole in it. It is refused and nothing is imported.
func TestLVMPVRefusesASetSplitBetweenTwoGroups(t *testing.T) {
	cmds := newLVM()
	cmds.byDevice = map[string]string{"/dev/nvme0n1": testVG, "/dev/nvme1n1": sourceVG}
	l := newLVMPV(cmds, lvmLabel(), nil)

	if _, err := l.Ensure(context.Background(), belowMembers(2)); err == nil {
		t.Fatalf("Ensure brought up a set split between two groups:\n%s", cmds.issued())
	}
	if imports := importsOf(cmds); len(imports) != 0 {
		t.Errorf("it imported %v anyway", imports)
	}
}

// A blank member beside a cloned one is not a clone being re-identified, and
// labeling it would join a new device to somebody's group. Refused, and neither
// labeled nor imported.
func TestLVMPVRefusesABlankMemberBesideACloned(t *testing.T) {
	cmds := newLVM()
	cmds.byDevice = map[string]string{"/dev/nvme1n1": sourceVG}
	l := NewLVMPhysicalVolume(LVMPhysicalVolumeConfig{
		VolumeGroup:   testVG,
		LogicalVolume: testLV,
		Manager:       cmds.manager(),
		Content: readingByDevice{
			"/dev/nvme0n1": {Content: blockdev.ContentBlank},
			"/dev/nvme1n1": lvmLabel(),
		},
	})

	if _, err := l.Ensure(context.Background(), belowMembers(2)); err == nil {
		t.Fatalf("Ensure brought up a blank member beside a cloned one:\n%s", cmds.issued())
	}
	if imports := importsOf(cmds); len(imports) != 0 {
		t.Errorf("it imported %v anyway", imports)
	}
	if labeled := issuedFor(cmds, "pvcreate"); len(labeled) != 0 {
		t.Errorf("it labeled %v anyway", labeled)
	}
}
