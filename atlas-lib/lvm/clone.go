package lvm

import (
	"context"
	"fmt"
)

// ImportClonedVolumeGroup regenerates fresh PV/VG UUIDs for devicePath and
// renames the VG to newVGName (vgimportclone), resolving the identity
// collision a byte-level clone or snapshot restore carries: it copies its
// source's on-disk PV/VG UUIDs and VG name verbatim, so without this the
// clone is indistinguishable from its source to LVM. The logical volume
// inside is left named after the source: rename it with RenameLogicalVolume.
func (m *Manager) ImportClonedVolumeGroup(ctx context.Context, devices []string, newVGName, devicePath string) error {
	_, err := m.Run(ctx, devices, "vgimportclone", "--basevgname", newVGName, devicePath)
	if err != nil {
		return fmt.Errorf("vgimportclone %s to %s: %w", devicePath, newVGName, err)
	}
	return nil
}

// RenameLogicalVolume renames the logical volume oldName, inside vg, to
// newName (lvrename). Needed after ImportClonedVolumeGroup, which leaves the
// logical volume named after the source.
func (m *Manager) RenameLogicalVolume(ctx context.Context, devices []string, vg, oldName, newName string) error {
	_, err := m.Run(ctx, devices, "lvrename", vg, oldName, newName)
	if err != nil {
		return fmt.Errorf("rename LV %s/%s to %s: %w", vg, oldName, newName, err)
	}
	return nil
}
