// What the volume-group layer has to guarantee.
//
// Membership and the host's hold, which are the two things that change without
// the volume inside changing. A group that gains a member has more room and the
// same volume in it; a group released on one host has every byte it had and no
// mapping anywhere. Both are why this is a layer of its own.

package layers

import (
	"context"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/volstack"
)

// newLVMGroup builds the layer over a fake LVM told what pvs answers for each
// device, keyed by device path so that members can differ from one another.
func newLVMGroup(perDevice map[string]string) (*LVMVolumeGroup, *lvmCommands) {
	cmds := newLVM()
	cmds.byDevice = perDevice
	return NewLVMVolumeGroup(LVMVolumeGroupConfig{
		VolumeGroup: testVG,
		Manager:     cmds.manager(),
	}), cmds
}

// ours is what pvs prints for a member of this volume's group.
func ours() string { return "  " + testVG + "\n" }

// Members carrying no group at all are what a bring-up creates one over.
func TestLVMVolumeGroupAbsentCreates(t *testing.T) {
	l, cmds := newLVMGroup(map[string]string{"/dev/nvme0n1": "\n", "/dev/nvme1n1": "\n"})
	below := belowMembers(2)

	state, own, err := l.Observe(context.Background(), below)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateAbsent {
		t.Fatalf("state = %s, want Absent", state)
	}
	if len(own.Devices) != 0 {
		t.Errorf("an absent layer exposed %d devices", len(own.Devices))
	}

	if _, err := l.Ensure(context.Background(), below); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !cmds.ran("vgcreate") {
		t.Fatalf("no group was created:\n%s", cmds.issued())
	}
	if !cmds.ran("vgchange") {
		t.Errorf("the group was created and never mapped on this host:\n%s", cmds.issued())
	}
}

// A group already spanning every member is ready, and Ensure maps it without
// changing what it is made of. Mapping is not skipped: a group that exists is
// not necessarily mapped here, which is exactly what a release leaves behind.
func TestLVMVolumeGroupReadyIsMappedNotRebuilt(t *testing.T) {
	l, cmds := newLVMGroup(map[string]string{"/dev/nvme0n1": ours(), "/dev/nvme1n1": ours()})
	below := belowMembers(2)

	state, own, err := l.Observe(context.Background(), below)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StateReady {
		t.Fatalf("state = %s, want Ready", state)
	}
	if len(own.Devices) != 2 {
		t.Errorf("exposed %d devices, want the two it was given", len(own.Devices))
	}

	if _, err := l.Ensure(context.Background(), below); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !cmds.ran("vgchange") {
		t.Errorf("a group that exists was not mapped, so a restage would find nothing:\n%s", cmds.issued())
	}
	for _, forbidden := range []string{"vgcreate", "vgextend"} {
		if cmds.ran(forbidden) {
			t.Errorf("a complete group was rebuilt with %s:\n%s", forbidden, cmds.issued())
		}
	}
}

// A member that has not joined is added rather than made the start of a second
// group. This is the case the layer exists for: a volume given more to work
// with, where the volume inside it has not changed at all.
func TestLVMVolumeGroupTakesOnANewMember(t *testing.T) {
	l, cmds := newLVMGroup(map[string]string{
		"/dev/nvme0n1": ours(),
		"/dev/nvme1n1": ours(),
		"/dev/nvme2n1": "\n",
		"/dev/nvme3n1": "\n",
	})
	below := belowMembers(4)

	state, _, err := l.Observe(context.Background(), below)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state != volstack.StatePartial {
		t.Fatalf("state = %s, want Partial: two members are in the group and two are not", state)
	}

	if _, err := l.Ensure(context.Background(), below); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !cmds.ran("vgextend") {
		t.Fatalf("the new members never joined the group:\n%s", cmds.issued())
	}
	if cmds.ran("vgcreate") {
		t.Fatalf("a second group was created beside the first:\n%s", cmds.issued())
	}
}

// Growing is the group's whole contribution to an expand, and it has two shapes.
// A member that grew is resized where it is; a member that is new joins. Both
// leave free extents for the layer above, and neither touches the volume.
func TestLVMVolumeGroupGrowsBothWays(t *testing.T) {
	t.Run("members that grew", func(t *testing.T) {
		l, cmds := newLVMGroup(map[string]string{"/dev/nvme0n1": ours(), "/dev/nvme1n1": ours()})

		if _, err := l.Grow(context.Background(), belowMembers(2)); err != nil {
			t.Fatalf("Grow: %v", err)
		}
		if got := issuedFor(cmds, "pvresize"); len(got) != 2 {
			t.Errorf("resized %v, want both members:\n%s", got, cmds.issued())
		}
		if cmds.ran("lvextend") {
			t.Errorf("the group extended the volume, which the layer above owns:\n%s", cmds.issued())
		}
	})

	t.Run("members that are new", func(t *testing.T) {
		l, cmds := newLVMGroup(map[string]string{
			"/dev/nvme0n1": ours(), "/dev/nvme1n1": "\n",
		})

		if _, err := l.Grow(context.Background(), belowMembers(2)); err != nil {
			t.Fatalf("Grow: %v", err)
		}
		if !cmds.ran("vgextend") {
			t.Fatalf("a new member was not taken into the group, so there is no new space:\n%s", cmds.issued())
		}
	})
}

// Release gives up the host's hold and keeps every byte. It is what an unstage
// calls, and an unstage fires on an ordinary pod restart.
func TestLVMVolumeGroupReleaseDeactivates(t *testing.T) {
	l, cmds := newLVMGroup(map[string]string{"/dev/nvme0n1": ours()})

	if err := l.Release(context.Background(), belowMembers(1)); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !cmds.ran("vgchange") {
		t.Errorf("the group was not deactivated:\n%s", cmds.issued())
	}
	for _, forbidden := range []string{"lvremove", "vgremove", "pvremove"} {
		if cmds.ran(forbidden) {
			t.Fatalf("Release ran %s, which an unstage must never do:\n%s", forbidden, cmds.issued())
		}
	}
}

// When the members are gone, LVM can no longer reach the metadata a clean
// deactivation needs, and a layer with no force path strands the stack below it.
func TestLVMVolumeGroupReleaseFallsBackToDeviceMapper(t *testing.T) {
	l, cmds := newLVMGroup(map[string]string{"/dev/nvme0n1": ours()})
	cmds.err["vgchange"] = errors.New("Volume group vol-... not found")

	if err := l.Release(context.Background(), belowMembers(1)); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !cmds.ran("dmsetup") {
		t.Fatalf("the force path never ran, so a dead stack has nothing left to clear it:\n%s", cmds.issued())
	}
}

// Destroy removes the group, and only a deletion path reaches it.
func TestLVMVolumeGroupDestroyRemovesTheGroup(t *testing.T) {
	l, cmds := newLVMGroup(map[string]string{"/dev/nvme0n1": ours()})

	if err := l.Destroy(context.Background(), belowMembers(1)); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !cmds.ran("vgremove") {
		t.Fatalf("no vgremove was issued:\n%s", cmds.issued())
	}
}

// A member carrying another volume's group is a clone the layer below
// re-identifies. Reading it as a member that has not joined would put a
// vgextend over somebody else's data.
func TestLVMVolumeGroupRefusesAForeignMember(t *testing.T) {
	l, cmds := newLVMGroup(map[string]string{
		"/dev/nvme0n1": ours(), "/dev/nvme1n1": "  vol-somebody-elses-volume\n",
	})

	if _, _, err := l.Observe(context.Background(), belowMembers(2)); err == nil {
		t.Error("Observe read another volume's group as a state of its own")
	}
	if _, err := l.Ensure(context.Background(), belowMembers(2)); err == nil {
		t.Error("Ensure proceeded over another volume's group")
	}
	for _, forbidden := range []string{"vgcreate", "vgextend"} {
		if cmds.ran(forbidden) {
			t.Fatalf("it ran %s over another volume's group:\n%s", forbidden, cmds.issued())
		}
	}
}

// A probe that failed is not a reading of absent, for the reason every layer
// here refuses one: LVM reports a device it could not read the same way it
// reports one carrying nothing.
func TestLVMVolumeGroupNeverCreatesOnAFailedProbe(t *testing.T) {
	l, cmds := newLVMGroup(map[string]string{"/dev/nvme0n1": "\n"})
	cmds.err["pvs"] = errors.New("cannot open /dev/nvme0n1 exclusively")

	if _, _, err := l.Observe(context.Background(), belowMembers(1)); err == nil {
		t.Error("Observe folded a probe failure into a state")
	}
	if cmds.ran("vgcreate") {
		t.Fatalf("it ran vgcreate anyway:\n%s", cmds.issued())
	}
}
