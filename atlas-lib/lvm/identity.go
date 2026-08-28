package lvm

import (
	"context"
	"slices"
	"strings"
)

// VolumeGroup returns the identity of the volume group pv's on-disk PV
// signature currently belongs to, or the zero VolumeGroup ("") if pv carries
// no LVM signature at all. A real probe failure (lock contention, an I/O
// error, a timeout) is returned as an error rather than folded into that same
// zero value: a caller reading it as "genuinely blank" would proceed straight
// to pvcreate/vgcreate over what might be live, merely unreadable data.
//
// Content-based (pvs on pv), not a name-based `vgs <name>` lookup: confirmed
// live that `vgs --devices devicePath vgname` can report success for a VG
// name that was never actually created on that device, on a host whose LVM
// devices file restricts default visibility to unrelated devices.
func (m *Manager) VolumeGroup(ctx context.Context, pv PhysicalVolume) (VolumeGroup, error) {
	out, err := m.exec(ctx, []string{pv.DevicePath}, "pvs", "--noheadings", "-o", "vg_name", pv.DevicePath)
	if err != nil {
		if isNoPVSignature(err) {
			return VolumeGroup{}, nil
		}
		return VolumeGroup{}, err
	}
	return VolumeGroup{Name: firstRealLine(out)}, nil
}

// isNoPVSignature reports whether err is pvs's own "this device carries no PV
// at all" failure, as opposed to a real probe error. pvs says so in its
// output text (e.g., "Failed to find physical volume ..."), not through a
// distinct exit code, so the text is what there is to go on (the same class
// of signal nvmeof.isAlreadyConnected reads for nvme-cli).
func isNoPVSignature(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "failed to find") || strings.Contains(msg, "not found")
}

// ListLogicalVolumes returns every logical volume volumeGroup currently
// contains.
func (m *Manager) ListLogicalVolumes(ctx context.Context, volumeGroup VolumeGroup) ([]LogicalVolume, error) {
	out, err := m.exec(ctx, nil, "lvs", "--noheadings", "-o", "lv_name", volumeGroup.Name)
	if err != nil {
		return nil, err
	}
	var lvs []LogicalVolume
	for name := range strings.FieldsSeq(out) {
		lvs = append(lvs, LogicalVolume{VolumeGroup: volumeGroup, Name: name})
	}
	return lvs, nil
}

// HasLogicalVolume reports whether logicalVolume's volume group already
// contains it. Distinguishes a fully assembled stack from one left orphaned
// by an interrupted create (pvcreate/vgcreate completed, the final lvcreate
// did not): such a VG reports zero LVs, and `vgchange -ay` against it
// "succeeds" while producing no mountable device at all.
//
// A real lvs failure is returned as an error, not folded into "zero LVs": a
// caller reaches here only after VolumeGroup has already confirmed the volume
// group exists on this device, so an lvs failure at this point is a genuine
// problem, not a signal that the volume is merely orphaned (misreading it as
// orphaned risks a caller destroying a real, valid VG over a transient probe
// failure).
func (m *Manager) HasLogicalVolume(ctx context.Context, logicalVolume LogicalVolume) (bool, error) {
	lvs, err := m.ListLogicalVolumes(ctx, logicalVolume.VolumeGroup)
	if err != nil {
		return false, err
	}
	return slices.Contains(lvs, logicalVolume), nil
}

// Rescan refreshes LVM's cached view of pvs (pvscan --cache), scoped to
// exactly those devices so that nothing else visible on the node is
// registered into LVM's cache as a side effect of inspecting them. That
// matters most for a freshly attached clone, whose PV UUID is still its
// source's until ImportClonedVolumeGroup has run.
func (m *Manager) Rescan(ctx context.Context, pvs ...PhysicalVolume) error {
	_, err := m.exec(ctx, devicePaths(pvs), "pvscan", "--cache")
	return err
}
