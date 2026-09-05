// What the volume layer has to guarantee.
//
// It is the layer that both creates and activates, and the whole of its risk is
// telling those apart. A volume group present but not mapped on this host is a
// volume to reactivate; the same group read as absent is a volume to create, and
// creating over the first destroys it.

package layers

import (
	"context"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// lvmVolumeFixture is the layer plus the commands it issued.
type lvmVolumeFixture struct {
	cmds  *lvmCommands
	layer *LVMVolume
}

// newLVMVolume builds the layer over a fake LVM, told what the device reports.
//
// vg is what pvs answers, lvs is what lvs answers for the volume group listing,
// and attr is the attribute string for this volume's own logical volume.
func newLVMVolume(vg, lvs, attr string, def lvm.LogicalVolumeDefinition) *lvmVolumeFixture {
	cmds := newLVM()
	cmds.out["pvs"] = vg
	cmds.out["lvs:lv_name"] = lvs
	cmds.out["lvs:lv_attr"] = attr
	return &lvmVolumeFixture{
		cmds: cmds,
		layer: NewLVMVolume(LVMVolumeConfig{
			VolumeGroup:   testVG,
			LogicalVolume: testLV,
			Definition:    def,
			Manager:       cmds.manager(),
			Resolve: func(path string) (blockdev.Device, error) {
				return blockdev.Device{Path: path, Name: "dm-0", LogicalBlockSize: 512, SizeBytes: 1 << 40}, nil
			},
		}),
	}
}

// present is the lvs listing for a volume group holding this volume.
func present() string { return "  " + testLV + "\n" }

// A volume group nothing has created yet is the only state a vgcreate may run in.
func TestLVMVolumeAbsentCreates(t *testing.T) {
	f := newLVMVolume("\n", "", "", lvm.LogicalVolumeDefinition{})

	state, own, err := f.layer.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateAbsent {
		t.Fatalf("state = %s, want Absent", state)
	}
	if len(own.Devices) != 0 {
		t.Errorf("an absent layer exposed %d devices", len(own.Devices))
	}

	if _, err := f.layer.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !f.cmds.ran("vgcreate") || !f.cmds.ran("lvcreate") {
		t.Fatalf("a create did not run both halves:\n%s", f.cmds.issued())
	}
}

// A complete volume group that is not mapped on this host is reactivated, and
// nothing about it is created. It is what a node reboot leaves behind, and what
// Release leaves behind, so this is the ordinary restage path.
func TestLVMVolumeInactiveActivatesAndCreatesNothing(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-------\n", lvm.LogicalVolumeDefinition{})

	state, _, err := f.layer.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateInactive {
		t.Fatalf("state = %s, want Inactive", state)
	}

	if _, err := f.layer.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !f.cmds.ran("vgchange") {
		t.Errorf("the volume group was not activated:\n%s", f.cmds.issued())
	}
	for _, forbidden := range []string{"vgcreate", "lvcreate", "pvcreate"} {
		if f.cmds.ran(forbidden) {
			t.Fatalf("a reactivation ran %s over an existing volume:\n%s", forbidden, f.cmds.issued())
		}
	}
}

// A volume group whose logical volume was never created is an interrupted create,
// not a volume to make a second group for. It reports zero logical volumes and
// activates successfully while producing no usable device, so every stage would
// otherwise reactivate an empty group forever.
func TestLVMVolumePartialCompletesTheCreate(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", "", "", lvm.LogicalVolumeDefinition{})

	state, _, err := f.layer.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StatePartial {
		t.Fatalf("state = %s, want Partial", state)
	}

	if _, err := f.layer.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !f.cmds.ran("lvcreate") {
		t.Errorf("the interrupted create was not completed:\n%s", f.cmds.issued())
	}
	if f.cmds.ran("vgcreate") {
		t.Fatalf("a second volume group was created over the first:\n%s", f.cmds.issued())
	}
}

// A mapped, complete volume is ready, and Ensure does nothing to it.
func TestLVMVolumeReadyIsLeftAlone(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})

	state, own, err := f.layer.Observe(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateReady {
		t.Fatalf("state = %s, want Ready", state)
	}
	dev, ok := own.Device()
	if !ok {
		t.Fatalf("a ready volume exposed %d devices, want one", len(own.Devices))
	}
	if want := "/dev/" + testVG + "/" + testLV; dev.Path != want {
		t.Errorf("exposed %s, want %s", dev.Path, want)
	}

	f.cmds.calls = nil
	if _, err := f.layer.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, forbidden := range []string{"vgcreate", "lvcreate", "vgchange"} {
		if f.cmds.ran(forbidden) {
			t.Errorf("a ready volume was acted on with %s:\n%s", forbidden, f.cmds.issued())
		}
	}
}

// Regression: 2026-09-05-vgcreate-over-a-foreign-volume-group. A device carrying
// another volume's group is a clone the layer below re-identifies, and this layer
// has no business deciding anything about it. Reading it as absent, which is what
// a probe that only asks "is my group here?" does, puts a vgcreate over a clone's
// data.
func TestLVMVolumeNeverCreatesOverAForeignGroup(t *testing.T) {
	f := newLVMVolume("  vol-somebody-elses-volume\n", "", "", lvm.LogicalVolumeDefinition{})

	if _, _, err := f.layer.Observe(context.Background(), belowArtifact()); err == nil {
		t.Error("Observe read another volume's group as a state of its own")
	}
	if _, err := f.layer.Ensure(context.Background(), belowArtifact()); err == nil {
		t.Error("Ensure proceeded over another volume's group")
	}
	for _, forbidden := range []string{"vgcreate", "lvcreate"} {
		if f.cmds.ran(forbidden) {
			t.Fatalf("it ran %s over another volume's group:\n%s", forbidden, f.cmds.issued())
		}
	}
}

// A probe that failed is not a reading of absent, for the reason the layer below
// refuses one: LVM reports a device it could not read the same way it reports one
// carrying nothing.
func TestLVMVolumeNeverCreatesOnAFailedProbe(t *testing.T) {
	f := newLVMVolume("", "", "", lvm.LogicalVolumeDefinition{})
	f.cmds.err["pvs"] = errors.New("cannot open /dev/nvme0n1 exclusively")

	if _, _, err := f.layer.Observe(context.Background(), belowArtifact()); err == nil {
		t.Error("Observe folded a probe failure into a state")
	}
	if _, err := f.layer.Ensure(context.Background(), belowArtifact()); err == nil {
		t.Error("Ensure proceeded on a device whose group is unknown")
	}
	if f.cmds.ran("vgcreate") {
		t.Fatalf("it ran vgcreate anyway:\n%s", f.cmds.issued())
	}
}

// Release deactivates and keeps the data. It is what an unstage calls, and an
// unstage fires on an ordinary pod restart.
func TestLVMVolumeReleaseDeactivates(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})

	if err := f.layer.Release(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !f.cmds.ran("vgchange") {
		t.Errorf("the volume group was not deactivated:\n%s", f.cmds.issued())
	}
	for _, forbidden := range []string{"lvremove", "vgremove", "pvremove"} {
		if f.cmds.ran(forbidden) {
			t.Fatalf("Release ran %s, which an unstage must never do:\n%s", forbidden, f.cmds.issued())
		}
	}
}

// When the backing device is gone, LVM can no longer read the metadata it needs
// to deactivate, and a layer with no force path strands the stack it sits on.
func TestLVMVolumeReleaseFallsBackToDeviceMapper(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})
	f.cmds.err["vgchange"] = errors.New("Volume group vol-... not found")

	if err := f.layer.Release(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !f.cmds.ran("dmsetup") {
		t.Fatalf("the force path never ran, so a dead stack has nothing left to clear it:\n%s", f.cmds.issued())
	}
}

// Destroy removes the volume and its data, in the order that leaves nothing
// behind: the logical volume first, then the group that held it.
func TestLVMVolumeDestroyRemovesBoth(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})

	if err := f.layer.Destroy(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	lvremove, vgremove := f.cmds.indexOf("lvremove"), f.cmds.indexOf("vgremove")
	if lvremove < 0 || vgremove < 0 {
		t.Fatalf("Destroy did not remove both:\n%s", f.cmds.issued())
	}
	if lvremove > vgremove {
		t.Errorf("the group was removed before the volume in it:\n%s", f.cmds.issued())
	}
}

// Grow extends the volume to the space its members gained, and is convergent:
// kubelet reissues NodeExpandVolume after one that already succeeded.
func TestLVMVolumeGrow(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})

	own, err := f.layer.Grow(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if !f.cmds.ran("pvresize") {
		t.Errorf("the member was never resized, so there is no new space to take:\n%s", f.cmds.issued())
	}
	if !f.cmds.ran("lvextend") {
		t.Errorf("the volume was never extended:\n%s", f.cmds.issued())
	}
	if _, ok := own.Device(); !ok {
		t.Error("a grown volume exposed no device")
	}
}

// A volume already at its target is a grow that succeeded, because that is what
// kubelet's retry finds.
func TestLVMVolumeGrowIsConvergent(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})
	f.cmds.err["lvextend"] = errors.New("New size (1535 extents) not larger than existing size (1535 extents)")

	if _, err := f.layer.Grow(context.Background(), belowArtifact()); err != nil {
		t.Errorf("Grow on a volume already at its target = %v, want nil", err)
	}
}

// The geometry the layer reports is the geometry it created, because it is read
// from the same definition lvcreate was built from. A filesystem aligned to a
// stripe that was never laid down is aligned to nothing.
func TestLVMVolumeReportsOnlyTheGeometryItCreated(t *testing.T) {
	cases := []struct {
		name  string
		def   lvm.LogicalVolumeDefinition
		known bool
	}{
		{"a striped volume", lvm.LogicalVolumeDefinition{Stripes: 4, StripeChunkBytes: 65536}, true},
		{"a linear volume", lvm.LogicalVolumeDefinition{}, false},
		{"a single member, which is not a stripe",
			lvm.LogicalVolumeDefinition{Stripes: 1, StripeChunkBytes: 65536}, false},
		{"a virtualized volume", lvm.LogicalVolumeDefinition{Deduplication: true, Compression: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", tc.def)

			_, own, err := f.layer.Observe(context.Background(), belowArtifact())
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if own.Geometry.Known() != tc.known {
				t.Errorf("geometry %+v reports Known() = %v, want %v", own.Geometry, own.Geometry.Known(), tc.known)
			}
		})
	}
}

// A volume whose type needs something from the node may be staged only where that
// something is, and the plan is what says so.
func TestLVMVolumeDeclaresItsNodeRequirement(t *testing.T) {
	f := newLVMVolume("\n", "", "", lvm.LogicalVolumeDefinition{})
	req, ok := any(f.layer).(volstack.NodeRequirements)
	if !ok {
		t.Fatal("the volume layer declares no node requirements")
	}
	if req.PinsToNode() {
		t.Error("the volume layer pins to a node, but its state is on the device, not the host")
	}

	capable := NewLVMVolume(LVMVolumeConfig{
		VolumeGroup: testVG, LogicalVolume: testLV,
		Capability: "vdo",
		Manager:    newLVM().manager(),
	})
	if got := capable.NodeCapability(); got != volstack.Capability("vdo") {
		t.Errorf("NodeCapability() = %q, want vdo", got)
	}
}

// The record carries what a later process needs in order to rebuild the layer,
// which for this one is what the volume was made to be. A teardown that rebuilt it
// from a StorageClass would read one edited since.
func TestLVMVolumeRecordsWhatItWasBuiltWith(t *testing.T) {
	def := lvm.LogicalVolumeDefinition{Stripes: 4, StripeChunkBytes: 65536}
	f := newLVMVolume("\n", "", "", def)

	recorder, ok := any(f.layer).(volstack.Recorder)
	if !ok {
		t.Fatal("the volume layer records nothing, so a teardown cannot rebuild it")
	}
	params, ok := recorder.Params().(LVMVolumeParams)
	if !ok {
		t.Fatalf("Params() = %T, want LVMVolumeParams", recorder.Params())
	}
	if params.Stripes != 4 || params.StripeChunkBytes != 65536 {
		t.Errorf("Params() = %+v, want the striping it was built with", params)
	}
}
