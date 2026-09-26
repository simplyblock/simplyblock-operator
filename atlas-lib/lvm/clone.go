package lvm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ImportClonedVolumeGroup regenerates fresh PV/VG UUIDs for pv and renames
// the volume group to newVolumeGroup (vgimportclone), resolving the identity
// collision a byte-level clone or snapshot restore carries: it copies its
// source's on-disk PV/VG UUIDs and VG name verbatim, so without this the
// clone is indistinguishable from its source to LVM. The logical volume inside
// is left named after the source: rename it with RenameLogicalVolume, or let
// ResolveClonedVolumeGroup drive the whole sequence, which is what a caller
// staging a freshly attached clone wants.
//
// Scoped to pv, and this is the one place in the package where that scoping
// decides an outcome rather than merely narrowing a scan. Until this command
// has run, the clone and its source answer to the same volume group name, so
// naming the device is the only thing that says which of the two to
// re-stamp.
func (m *Manager) ImportClonedVolumeGroup(ctx context.Context, newVolumeGroup VolumeGroup, pv PhysicalVolume) error {
	if err := m.requireOwnedDevice(ctx, pv); err != nil {
		return err
	}
	return m.importClone(ctx, newVolumeGroup, pv)
}

// importClone is the import itself, for a caller that has just established
// ownership: ResolveClonedVolumeGroup, after it adopted a source that predates
// the tag.
func (m *Manager) importClone(ctx context.Context, newVolumeGroup VolumeGroup, pv PhysicalVolume) error {
	_, err := m.exec(ctx, []string{pv.DevicePath}, "vgimportclone", "--basevgname", newVolumeGroup.Name, pv.DevicePath)
	if err != nil {
		return fmt.Errorf("vgimportclone %s to %s: %w", pv.DevicePath, newVolumeGroup.Name, err)
	}
	return nil
}

// RenameLogicalVolume renames the logical volume oldName, inside volumeGroup,
// to newName (lvrename). Needed after ImportClonedVolumeGroup, which leaves
// the logical volume named after the source, and driven for you by
// ResolveClonedVolumeGroup.
//
// Unscoped: by the time this runs, ImportClonedVolumeGroup has already given
// the clone a volume group name of its own, so the name identifies it.
func (m *Manager) RenameLogicalVolume(ctx context.Context, volumeGroup VolumeGroup, oldName, newName string) error {
	if err := m.requireOwned(ctx, volumeGroup); err != nil {
		return err
	}
	_, err := m.exec(ctx, nil, "lvrename", volumeGroup.Name, oldName, newName)
	if err != nil {
		return fmt.Errorf("rename LV %s/%s to %s: %w", volumeGroup.Name, oldName, newName, err)
	}
	return nil
}

// ResolveClonedVolumeGroup gives pv an identity of its own when it turns out
// to be a byte-level clone or snapshot restore of another volume, and reports
// the foreign volume group it found, or the zero VolumeGroup when there was
// nothing to resolve. It is the whole sequence a caller staging a freshly
// attached device needs: refresh LVM's view of the device, ask the device what
// it is, and if the answer is somebody else's volume group, re-stamp it and
// rename the logical volume inside to logicalVolume.
//
// The orchestration lives here rather than in a caller because each step's
// reason is a property of LVM, not of the caller: the refresh has to precede
// the probe or the probe reads a stale cache, the probe has to be
// content-based or it cannot see a foreign identity at all (the volume group
// on disk is still named after the source), and the rename has to follow the
// import because vgimportclone renames the volume group but leaves the logical
// volume inside named after the source.
//
// A device that is blank, or that already carries volumeGroup, is left alone
// and reports the zero VolumeGroup. Names in preserve are left alone too, for
// the structural logical volumes a stack creates for itself and names the
// same way in every volume (a VDO pool, say), so that exactly the one logical
// volume carrying the source's name is renamed.
//
// Whether pv is a clone at all is not something a caller has to know in
// advance: this is safe and cheap to call on any freshly attached device,
// which is why it reads the identity itself rather than taking a "this is a
// clone" flag.
func (m *Manager) ResolveClonedVolumeGroup(
	ctx context.Context, pv PhysicalVolume, volumeGroup VolumeGroup, logicalVolume string,
	recognize StackRecognizer, preserve ...string,
) (VolumeGroup, error) {
	// Best-effort: pvscan --cache only refreshes what LVM has cached, and the
	// probe below reads pv's content directly, so a failed refresh costs
	// nothing that the probe does not recover.
	_ = m.Rescan(ctx, pv)

	current, err := m.VolumeGroup(ctx, pv)
	if err != nil {
		return VolumeGroup{}, fmt.Errorf("probe VG identity of %s: %w", pv.DevicePath, err)
	}
	if current.Name == "" || current == volumeGroup {
		return VolumeGroup{}, nil
	}

	// The group is somebody's until it is shown to be the driver's: a clone of a
	// tagged volume carries the tag already, and one of a volume made before the
	// tag existed is known by its names alone. Anything else is refused here,
	// with the group untouched, which is the whole point of the tag.
	if err := m.requireOwnedDevice(ctx, pv); err != nil {
		if !errors.Is(err, ErrNotOwned) {
			return VolumeGroup{}, err
		}
		volumes, listErr := m.listLogicalVolumesOn(ctx, pv, current)
		if listErr != nil {
			return VolumeGroup{}, fmt.Errorf("list the volumes in %s on %s: %w", current.Name, pv.DevicePath, listErr)
		}
		if recognize == nil || !recognize(current.Name, volumes) {
			return VolumeGroup{}, fmt.Errorf(
				"%w: %s on %s holds %v and is not a stack of this driver's, so it is not re-identified",
				ErrNotOwned, current.Name, pv.DevicePath, volumes)
		}
		if err := m.adoptOnDevice(ctx, pv, current); err != nil {
			return VolumeGroup{}, err
		}
	}

	if err := m.importClone(ctx, volumeGroup, pv); err != nil {
		return VolumeGroup{}, err
	}

	lvs, err := m.ListLogicalVolumes(ctx, volumeGroup)
	if err != nil {
		return VolumeGroup{}, fmt.Errorf("list LVs in %s after clone resolution: %w", volumeGroup.Name, err)
	}
	for _, lv := range lvs {
		if lv.Name == logicalVolume || slices.Contains(preserve, lv.Name) {
			continue
		}
		if err := m.RenameLogicalVolume(ctx, volumeGroup, lv.Name, logicalVolume); err != nil {
			return VolumeGroup{}, err
		}
		break
	}
	return current, nil
}

// listLogicalVolumesOn is ListLogicalVolumes scoped to one device, for a group
// whose name and UUID a source elsewhere on the host may share.
func (m *Manager) listLogicalVolumesOn(ctx context.Context, pv PhysicalVolume, volumeGroup VolumeGroup) ([]string, error) {
	out, err := m.exec(ctx, []string{pv.DevicePath}, "lvs", "--noheadings", "-o", "lv_name", volumeGroup.Name)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}
