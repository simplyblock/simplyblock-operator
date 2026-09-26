// What the logical-volume layer has to guarantee.
//
// The whole of its risk is telling a volume that exists from one that does not.
// A volume present but not mapped on this host is one to reactivate; the same
// volume read as absent is one to create, and creating over the first destroys
// it. The group it lives in, and which members that group is made of, belong to
// the layer below.

package layers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// lvmVolumeFixture is the layer plus the commands it issued.
type lvmVolumeFixture struct {
	cmds  *lvmCommands
	layer *LVMLogicalVolume
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
		layer: NewLVMLogicalVolume(LVMLogicalVolumeConfig{
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

// A group holding no volume of ours is the only state an lvcreate may run in.
// The group itself is already there by then, made by the layer below.
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
	if !f.cmds.ran("lvcreate") {
		t.Fatalf("the volume was never created:\n%s", f.cmds.issued())
	}
	if f.cmds.ran("vgcreate") {
		t.Fatalf("it made a group, which the layer below owns:\n%s", f.cmds.issued())
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

// Total path loss leaves this layer nothing to read a group off at all, which
// is a different question from a probe that failed on a device that is there.
// Release is unconditionally a no-op for this layer regardless of state, so
// reporting Absent here costs nothing, and it is what lets a Down walk reach
// the group layer below without this layer's own inability to check anything
// aborting the walk first. (Runner.survey walks bottom to top, so in practice
// the group layer below answers this question before this layer is even
// asked, but this layer must not depend on that to stay safe on its own.)
func TestLVMVolumeObserveWithNoDeviceReportsAbsentWithoutError(t *testing.T) {
	f := newLVMVolume("", "", "", lvm.LogicalVolumeDefinition{})

	state, own, err := f.layer.Observe(context.Background(), volstack.Artifact{})
	if err != nil {
		t.Fatalf("Observe: %v, want no error when there is no device to read at all", err)
	}
	if state != volstack.StateAbsent {
		t.Errorf("state = %s, want Absent", state)
	}
	if len(own.Devices) != 0 {
		t.Errorf("an absent layer exposed %d devices", len(own.Devices))
	}
	if len(f.cmds.calls) != 0 {
		t.Errorf("Observe ran LVM commands with no device to scope them to:\n%s", f.cmds.issued())
	}
}

// Release does nothing here. What holds a logical volume on a host is its group
// being mapped there, and the group is the layer below: a teardown walks down
// through both, so the hold is given up either way, and giving it up here as
// well would take everything else in the group down with this one volume.
func TestLVMVolumeReleaseLeavesTheGroupToTheLayerBelow(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})

	if err := f.layer.Release(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(f.cmds.calls) != 0 {
		t.Errorf("Release ran something, and an unstage reaches it:\n%s", f.cmds.issued())
	}
}

// Destroy removes the volume and the data in it, and leaves the group to the
// layer below, which a teardown reaches next.
func TestLVMVolumeDestroyRemovesTheVolume(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})

	if err := f.layer.Destroy(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !f.cmds.ran("lvremove") {
		t.Fatalf("Destroy did not remove the volume:\n%s", f.cmds.issued())
	}
	if f.cmds.ran("vgremove") {
		t.Errorf("Destroy removed the group, which belongs to the layer below:\n%s", f.cmds.issued())
	}
}

// Grow takes the space the group below now has, and only takes it: making the
// space is the group's, whether that meant resizing members or accepting new
// ones.
func TestLVMVolumeGrow(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-a-----\n", lvm.LogicalVolumeDefinition{})

	own, err := f.layer.Grow(context.Background(), belowArtifact())
	if err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if f.cmds.ran("pvresize") {
		t.Errorf("the volume resized a member, which the group below owns:\n%s", f.cmds.issued())
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

	capable := NewLVMLogicalVolume(LVMLogicalVolumeConfig{
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
	params, ok := recorder.Params().(LVMLogicalVolumeParams)
	if !ok {
		t.Fatalf("Params() = %T, want LVMLogicalVolumeParams", recorder.Params())
	}
	if params.Stripes != 4 || params.StripeChunkBytes != 65536 {
		t.Errorf("Params() = %+v, want the striping it was built with", params)
	}
}

// wantMarker is the tag the layer puts on the volume group before it runs
// lvcreate and removes once lvcreate has returned. It is the on-disk contract a
// recovery reads, so the tests spell it out rather than sharing the constant
// with the layer: a renamed constant that changed what is written to disk would
// otherwise pass every test here.
const wantMarker = "simplyblock.creating"

// testPool is the pool a VDO-backed volume is created inside.
const testPool = "vdopool"

// newPooledLVMVolume is newLVMVolume for a volume of a pooled type, told what
// vgs answers for the group's tags as well.
func newPooledLVMVolume(lvs, tags string) *lvmVolumeFixture {
	f := newLVMVolume("  "+testVG+"\n", lvs, "", lvm.LogicalVolumeDefinition{Deduplication: true})
	f.cmds.out["vgs:vg_tags"] = tags
	f.layer.cfg.PoolName = testPool
	return f
}

// indexOfWith is where a command carrying this argument was issued, or -1.
func (l *lvmCommands) indexOfWith(command, arg string) int {
	for i, call := range l.calls {
		if call[0] != command {
			continue
		}
		for _, a := range call[1:] {
			if a == arg {
				return i
			}
		}
	}
	return -1
}

// Regression: 2026-09-26-vdopool-already-exists-after-interrupted-lvcreate. A
// create is several LVM commits for a pooled type, and a node can die between
// them. The layer says so before it starts, on the group itself, so that whoever
// finds the leftovers knows they are an interrupted create of ours and not
// somebody's data. The marker goes on before lvcreate and comes off after it,
// and a recovery that finds it still there knows lvcreate never finished.
func TestLVMVolumeCreateMarksTheGroupAroundLvcreate(t *testing.T) {
	f := newLVMVolume("\n", "", "", lvm.LogicalVolumeDefinition{})

	if _, err := f.layer.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	added := f.cmds.indexOfWith("vgchange", "--addtag")
	created := f.cmds.indexOf("lvcreate")
	removed := f.cmds.indexOfWith("vgchange", "--deltag")
	if added < 0 || created < 0 || removed < 0 {
		t.Fatalf("want the marker added, the volume created, and the marker removed:\n%s", f.cmds.issued())
	}
	if added >= created || created >= removed {
		t.Fatalf("the marker has to bracket lvcreate, and instead:\n%s", f.cmds.issued())
	}
	if f.cmds.indexOfWith("vgchange", wantMarker) < 0 {
		t.Fatalf("the marker written is not %q:\n%s", wantMarker, f.cmds.issued())
	}
}

// Regression: 2026-09-26-vdopool-already-exists-after-interrupted-lvcreate. The
// interrupted create of a pooled type leaves the pool behind under the very
// name the retry needs, so completing the create means removing it first. That
// is allowed on exactly one reading of the group: it holds the pool and nothing
// else, and it carries the marker this layer put there before lvcreate. Nothing
// can have written into a pool that has no volume inside it, and the marker says
// whose interrupted work it is.
func TestLVMVolumeRecoversItsOwnInterruptedPoolCreate(t *testing.T) {
	f := newPooledLVMVolume("  "+testPool+"\n", "  "+wantMarker+"\n")

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
	removed := f.cmds.indexOfWith("lvremove", testVG+"/"+testPool)
	created := f.cmds.indexOf("lvcreate")
	if removed < 0 || created < 0 {
		t.Fatalf("want the orphan pool removed and the create rerun:\n%s", f.cmds.issued())
	}
	if removed > created {
		t.Fatalf("the pool has to go before lvcreate can take its name:\n%s", f.cmds.issued())
	}
	if f.cmds.indexOfWith("vgchange", "--deltag") < created {
		t.Fatalf("the marker came off before the create finished:\n%s", f.cmds.issued())
	}
}

// Regression: 2026-09-26-vdopool-already-exists-after-interrupted-lvcreate. A
// pool with no marker is a shape this layer did not make, whatever it looks
// like, and the only safe thing to do with it is nothing. The refusal has to
// say what it found, since the alternative is a stage that retries forever with
// LVM's own message.
func TestLVMVolumeRefusesAPoolWithoutItsMarker(t *testing.T) {
	f := newPooledLVMVolume("  "+testPool+"\n", "\n")

	_, err := f.layer.Ensure(context.Background(), belowArtifact())
	if err == nil {
		t.Fatalf("a pool of unknown origin was converged over:\n%s", f.cmds.issued())
	}
	for _, forbidden := range []string{"lvremove", "lvcreate"} {
		if f.cmds.ran(forbidden) {
			t.Fatalf("the refusal ran %s:\n%s", forbidden, f.cmds.issued())
		}
	}
	if !strings.Contains(err.Error(), testPool) {
		t.Errorf("the refusal does not name what it found: %v", err)
	}
}

// Regression: 2026-09-26-vdopool-already-exists-after-interrupted-lvcreate. A
// pool with another volume beside it is somebody's data: a clone whose volume
// has not been renamed yet, or a volume a human renamed. The marker being there
// changes nothing, because the marker vouches for an empty group and this one is
// not empty. Nothing is removed, and nothing is created beside it either.
func TestLVMVolumeRefusesAPoolBesideAnotherVolume(t *testing.T) {
	f := newPooledLVMVolume("  "+testPool+"\n  source-lv\n", "  "+wantMarker+"\n")

	_, err := f.layer.Ensure(context.Background(), belowArtifact())
	if err == nil {
		t.Fatalf("a group holding another volume was converged over:\n%s", f.cmds.issued())
	}
	for _, forbidden := range []string{"lvremove", "lvcreate"} {
		if f.cmds.ran(forbidden) {
			t.Fatalf("the refusal ran %s:\n%s", forbidden, f.cmds.issued())
		}
	}
	if !strings.Contains(err.Error(), "source-lv") {
		t.Errorf("the refusal does not name what it found: %v", err)
	}
}

// Regression: 2026-09-26-vdopool-already-exists-after-interrupted-lvcreate. The
// same holds for a volume of a plain type: our group, holding a volume that
// is not ours, is not an interrupted create to complete. It is somebody's, and
// an lvcreate into it is at best a failure over free space and at worst a
// second volume beside data nobody declared.
func TestLVMVolumeRefusesAForeignVolumeInItsGroup(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", "  somebody-elses\n", "", lvm.LogicalVolumeDefinition{})

	_, err := f.layer.Ensure(context.Background(), belowArtifact())
	if err == nil {
		t.Fatalf("a group holding a foreign volume was created into:\n%s", f.cmds.issued())
	}
	if f.cmds.ran("lvcreate") {
		t.Fatalf("the refusal ran lvcreate:\n%s", f.cmds.issued())
	}
}

// Regression: 2026-09-26-vdopool-already-exists-after-interrupted-lvcreate. The
// marker comes off after lvcreate, and that removal can fail with the volume
// already made. A group carrying the marker with its volume in it is then
// complete, and the marker is stale: nothing about it is an interrupted create,
// and a clone of it, or a later shape, must not read the marker as permission.
// The next bring-up clears it on the way through, creating nothing.
func TestLVMVolumeClearsAStaleMarkerFromACompleteGroup(t *testing.T) {
	f := newLVMVolume("  "+testVG+"\n", present(), "  -wi-------\n", lvm.LogicalVolumeDefinition{})
	f.cmds.out["vgs:vg_tags"] = "  " + wantMarker + "\n"

	if _, err := f.layer.Ensure(context.Background(), belowArtifact()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if f.cmds.indexOfWith("vgchange", "--deltag") < 0 {
		t.Fatalf("the stale marker was left on a complete group:\n%s", f.cmds.issued())
	}
	for _, forbidden := range []string{"lvcreate", "lvremove"} {
		if f.cmds.ran(forbidden) {
			t.Fatalf("clearing a marker ran %s:\n%s", forbidden, f.cmds.issued())
		}
	}
}
