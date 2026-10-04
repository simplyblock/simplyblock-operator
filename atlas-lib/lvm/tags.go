// Tags on a volume group: the one piece of state LVM keeps on the device that
// this package can write without creating or destroying anything.
//
// A layer uses them to say, ahead of a multi-commit create, that it is in the
// middle of one. The tag lives in the group's metadata, so it survives the node
// rebooting, follows the volume to whichever node stages it next, and is copied
// into a clone along with everything else, which is what lets a later bring-up
// tell its own interrupted work from somebody's data.

package lvm

import (
	"context"
	"fmt"
	"strings"
)

// AddVolumeGroupTag puts tag on volumeGroup (vgchange --addtag).
//
// Convergent: adding a tag the group already carries is not an error to LVM,
// which is what a create resuming after a crash depends on.
func (m *Manager) AddVolumeGroupTag(ctx context.Context, volumeGroup VolumeGroup, tag string) error {
	if err := m.requireOwned(ctx, volumeGroup); err != nil {
		return err
	}
	if _, err := m.exec(ctx, nil, "vgchange", "--addtag", tag, volumeGroup.Name); err != nil {
		return fmt.Errorf("vgchange --addtag %s %s: %w", tag, volumeGroup.Name, err)
	}
	return nil
}

// RemoveVolumeGroupTag takes tag off volumeGroup (vgchange --deltag).
//
// Convergent for the same reason: LVM accepts removing a tag that is not there.
func (m *Manager) RemoveVolumeGroupTag(ctx context.Context, volumeGroup VolumeGroup, tag string) error {
	if err := m.requireOwned(ctx, volumeGroup); err != nil {
		return err
	}
	if _, err := m.exec(ctx, nil, "vgchange", "--deltag", tag, volumeGroup.Name); err != nil {
		return fmt.Errorf("vgchange --deltag %s %s: %w", tag, volumeGroup.Name, err)
	}
	return nil
}

// VolumeGroupTags is every tag volumeGroup carries, in the order LVM lists
// them. A group with none answers an empty slice.
//
// A real vgs failure is returned rather than read as a group without tags, for
// the reason HasLogicalVolume gives: a caller here has already confirmed the group exists,
// and a reading of "no marker" that came from a failed command would have it
// refuse a recovery it was entitled to, or worse, trust one it was not.
func (m *Manager) VolumeGroupTags(ctx context.Context, volumeGroup VolumeGroup) ([]string, error) {
	out, err := m.exec(ctx, nil, "vgs", "--noheadings", "-o", "vg_tags", volumeGroup.Name)
	if err != nil {
		return nil, fmt.Errorf("vgs -o vg_tags %s: %w", volumeGroup.Name, err)
	}
	var tags []string
	for tag := range strings.SplitSeq(firstRealLine(out), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags, nil
}
