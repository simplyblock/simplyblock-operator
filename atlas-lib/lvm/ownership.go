// Ownership: the tag every group and volume this driver makes carries, and the
// check every mutation in this package runs before it touches a group.
//
// A name is derivable by anybody and says nothing about origin. The tag is what
// says a group is the driver's, and it lives in the group's own metadata, so it
// survives a reboot, follows the volume to whichever node stages it next, and is
// copied into a clone by vgimportclone. A mutation on a group without it is
// refused with ErrNotOwned before any command that changes the group is issued.
//
// Two mutations cannot check, and say so where they are: pvcreate, because the
// device carries no group yet and the layer above decides by reading the device's
// content, and the device-mapper force path, which unmaps nodes on this host
// after LVM itself has stopped answering and changes nothing on the device.

package lvm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// OwnerTag marks a volume group, and the logical volumes inside it, as made by
// the simplyblock CSI driver. Written as `@storage.simplyblock.io` when a command
// selects by it.
const OwnerTag = "storage.simplyblock.io"

// CreatingMarker is the tag a group carries while lvcreate runs, put there and
// taken off by the lvmLogicalVolume layer. It shares OwnerTag's namespace so that
// everything the driver writes into LVM metadata is one prefix.
const CreatingMarker = OwnerTag + "/creating"

// ErrNotOwned is returned by every mutation asked to act on a group that does
// not carry OwnerTag. The group is left as it was.
var ErrNotOwned = errors.New("lvm: the volume group does not carry " + OwnerTag + ", so it is not this driver's to change")

// errGroupGone is what requireOwned reports when there is no group to check.
// A removal reads it as already done. Everything else reads it as the failure
// LVM would have reported anyway.
var errGroupGone = errors.New("lvm: the volume group is not there")

// StackRecognizer answers whether a group made before OwnerTag existed is one of
// the driver's, from its name and the names of the volumes in it. The driver's
// naming lives above this package, which is why the question is a parameter.
type StackRecognizer func(volumeGroup string, logicalVolumes []string) bool

// requireOwned refuses to go on unless volumeGroup carries OwnerTag.
func (m *Manager) requireOwned(ctx context.Context, volumeGroup VolumeGroup) error {
	tags, err := m.VolumeGroupTags(ctx, volumeGroup)
	if err != nil {
		if isAlreadyGone(err) {
			return fmt.Errorf("%w: %s", errGroupGone, volumeGroup.Name)
		}
		return err
	}
	if !slices.Contains(tags, OwnerTag) {
		return fmt.Errorf("%w: %s", ErrNotOwned, volumeGroup.Name)
	}
	return nil
}

// requireOwnedDevice is requireOwned for a command that addresses a device
// rather than a group. A device carrying no group, or no label at all, has
// nothing to protect and passes: the removals behind this are convergent, and
// the layer above has already decided what may be labeled.
func (m *Manager) requireOwnedDevice(ctx context.Context, pv PhysicalVolume) error {
	out, err := m.exec(ctx, []string{pv.DevicePath}, "pvs", "--noheadings", "-o", "vg_name,vg_tags", pv.DevicePath)
	if err != nil {
		if isNoPVSignature(err) {
			return nil
		}
		return err
	}
	fields := strings.Fields(firstRealLine(out))
	if len(fields) == 0 || fields[0] == "" {
		return nil
	}
	if len(fields) > 1 && slices.Contains(strings.Split(fields[1], ","), OwnerTag) {
		return nil
	}
	return fmt.Errorf("%w: %s on %s", ErrNotOwned, fields[0], pv.DevicePath)
}

// AdoptVolumeGroup puts OwnerTag on volumeGroup and on every volume in it, for a
// group the caller has established is the driver's by other means: one made
// before the tag existed. It is the one write in this package that does not
// require the tag first, and the caller's evidence is the whole of its safety.
//
// The volumes are tagged first and the group last, so that the group's tag is
// the commit: an adoption that died between the two leaves a group that still
// reads as unowned, and the next bring-up runs the whole of it again. Tagging a
// volume twice is nothing to LVM.
func (m *Manager) AdoptVolumeGroup(ctx context.Context, volumeGroup VolumeGroup) error {
	return m.adopt(ctx, nil, volumeGroup)
}

// adoptOnDevice is AdoptVolumeGroup scoped to one device, for a clone whose
// group still carries its source's name and UUID: unscoped, the command could
// find the source instead.
func (m *Manager) adoptOnDevice(ctx context.Context, pv PhysicalVolume, volumeGroup VolumeGroup) error {
	return m.adopt(ctx, []string{pv.DevicePath}, volumeGroup)
}

func (m *Manager) adopt(ctx context.Context, devices []string, volumeGroup VolumeGroup) error {
	if _, err := m.exec(ctx, devices, "lvchange", "--addtag", OwnerTag, volumeGroup.Name); err != nil {
		return fmt.Errorf("adopt the volumes in VG %s: %w", volumeGroup.Name, err)
	}
	if _, err := m.exec(ctx, devices, "vgchange", "--addtag", OwnerTag, volumeGroup.Name); err != nil {
		return fmt.Errorf("adopt VG %s: %w", volumeGroup.Name, err)
	}
	return nil
}

// InformationalTagKeys are the tags this driver writes for whoever reads a
// node's LVM metadata: which volume, which PersistentVolume, and which claim a
// group belongs to. They are written as `<OwnerTag>/<key>=<value>`, refreshed on
// every bring-up because a claim can be rebound, and never used as a gate: the
// only tag that decides anything is OwnerTag.
var InformationalTagKeys = []string{"lvol", "pv", "pvc"}

// InformationalTag renders one informational tag.
func InformationalTag(key, value string) string { return OwnerTag + "/" + key + "=" + value }

// isInformationalTag reports whether tag is one this driver writes for
// information, as opposed to OwnerTag, CreatingMarker, or somebody else's.
func isInformationalTag(tag string) bool {
	for _, key := range InformationalTagKeys {
		if strings.HasPrefix(tag, OwnerTag+"/"+key+"=") {
			return true
		}
	}
	return false
}

// ReconcileInformationalTags makes volumeGroup's informational tags exactly
// want: tags of that kind it carries and want does not are removed, and tags in
// want it lacks are added. OwnerTag, CreatingMarker, and any tag that is not
// this driver's are left alone. A group already carrying want issues nothing.
func (m *Manager) ReconcileInformationalTags(ctx context.Context, volumeGroup VolumeGroup, want []string) error {
	tags, err := m.VolumeGroupTags(ctx, volumeGroup)
	if err != nil {
		return err
	}
	if !slices.Contains(tags, OwnerTag) {
		return fmt.Errorf("%w: %s", ErrNotOwned, volumeGroup.Name)
	}
	for _, tag := range tags {
		if isInformationalTag(tag) && !slices.Contains(want, tag) {
			if _, err := m.exec(ctx, nil, "vgchange", "--deltag", tag, volumeGroup.Name); err != nil {
				return fmt.Errorf("vgchange --deltag %s %s: %w", tag, volumeGroup.Name, err)
			}
		}
	}
	for _, tag := range want {
		if !slices.Contains(tags, tag) {
			if _, err := m.exec(ctx, nil, "vgchange", "--addtag", tag, volumeGroup.Name); err != nil {
				return fmt.Errorf("vgchange --addtag %s %s: %w", tag, volumeGroup.Name, err)
			}
		}
	}
	return nil
}
