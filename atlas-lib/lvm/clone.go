package lvm

import (
	"context"
	"fmt"
	"slices"
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
	ctx context.Context, pv PhysicalVolume, volumeGroup VolumeGroup, logicalVolume string, preserve ...string,
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

	if err := m.ImportClonedVolumeGroup(ctx, volumeGroup, pv); err != nil {
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
