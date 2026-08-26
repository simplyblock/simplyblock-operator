package lvm

import (
	"context"
	"slices"
	"strings"
)

// VolumeGroup returns the name of the volume group devicePath's on-disk PV
// signature currently belongs to, or "" if devicePath carries no LVM
// signature at all. A genuinely blank device, or the probe itself failing,
// are both treated the same way by callers: nothing to resolve.
//
// Content-based (pvs on devicePath), not a name-based `vgs <name>` lookup:
// confirmed live that `vgs --devices devicePath vgname` can report success
// for a VG name that was never actually created on that device, on a host
// whose LVM devices file restricts default visibility to unrelated devices.
func (m *Manager) VolumeGroup(ctx context.Context, devicePath string) (string, error) {
	out, err := m.Run(ctx, []string{devicePath}, "pvs", "--noheadings", "-o", "vg_name", devicePath)
	if err != nil {
		return "", nil //nolint:nilerr // probe failure reads as "nothing to resolve," same as a blank device
	}
	return firstRealLine(out), nil
}

// ListLogicalVolumes returns the names of every logical volume vg, scoped to
// devices, currently contains.
func (m *Manager) ListLogicalVolumes(ctx context.Context, devices []string, vg string) ([]string, error) {
	out, err := m.Run(ctx, devices, "lvs", "--noheadings", "-o", "lv_name", vg)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range strings.FieldsSeq(out) {
		names = append(names, name)
	}
	return names, nil
}

// HasLogicalVolume reports whether vg, scoped to devices, already contains a
// logical volume named lvName. Distinguishes a fully assembled stack from one
// left orphaned by an interrupted create (pvcreate/vgcreate completed, the
// final lvcreate did not): such a VG reports zero LVs, and `vgchange -ay`
// against it "succeeds" while producing no mountable device at all.
func (m *Manager) HasLogicalVolume(ctx context.Context, devices []string, vg, lvName string) (bool, error) {
	names, err := m.ListLogicalVolumes(ctx, devices, vg)
	if err != nil {
		return false, nil //nolint:nilerr // unreadable VG reads as "nothing found," same as a genuinely empty one
	}
	return slices.Contains(names, lvName), nil
}

// Rescan refreshes LVM's cached view of devices (pvscan --cache), scoped to
// exactly these devices so a volume's other redundant local device nodes are
// never registered into LVM's cache alongside the ones being inspected.
func (m *Manager) Rescan(ctx context.Context, devices []string) error {
	_, err := m.Run(ctx, devices, "pvscan", "--cache")
	return err
}
