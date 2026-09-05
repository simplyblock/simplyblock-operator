package lvm

import (
	"context"
	"fmt"
	"strings"
)

// ExpandPhysicalVolume grows pv to its device's current full size (pvresize),
// after the backing device itself has grown.
//
// Unscoped although it names a device, unlike CreatePhysicalVolume: pvresize
// rewrites the metadata of the volume group the PV belongs to, which lives on
// every member of that group, so a scope narrowed to this one device would
// hide the rest of the group from a command that has to write to it.
func (m *Manager) ExpandPhysicalVolume(ctx context.Context, pv PhysicalVolume) error {
	_, err := m.exec(ctx, nil, "pvresize", pv.DevicePath)
	if err != nil {
		return fmt.Errorf("pvresize %s: %w", pv.DevicePath, err)
	}
	return nil
}

// ExtendVolumeGroup adds pvs to volumeGroup (vgextend). The counterpart to
// CreateVolumeGroup for a volume group that already exists: growing a striped
// volume group by adding members, as opposed to ExpandPhysicalVolume, which
// grows a PV already in the volume group to its device's current full size.
//
// Unscoped, for the reason ExpandPhysicalVolume gives: the command has to
// resolve the existing volume group, whose current members are not among
// pvs.
func (m *Manager) ExtendVolumeGroup(ctx context.Context, volumeGroup VolumeGroup, pvs ...PhysicalVolume) error {
	paths := devicePaths(pvs)
	args := append([]string{"vgextend", volumeGroup.Name}, paths...)
	if _, err := m.exec(ctx, nil, args...); err != nil {
		return fmt.Errorf("vgextend %s with %v: %w", volumeGroup.Name, paths, err)
	}
	return nil
}

// ExpandLogicalVolume grows logicalVolume to consume all newly available free
// space in its volume group (lvextend -l+100%FREE). Always the full free
// space: unlike ExtendLogicalVolumeToSize, there is no partial-expand use
// case for this package's callers.
//
// The "+" prefix matters: lvextend's -l (unlike lvcreate's) treats a bare
// "100%FREE" as an ABSOLUTE target size (100% of what's currently free), not
// "grow by." Confirmed live: "New size given (1024 extents) not larger than
// existing size (1535 extents)," since free-space-alone is smaller than the
// volume's current size. "+100%FREE" is additive (current size + free
// space), which is what "grow to consume all newly available space" means.
func (m *Manager) ExpandLogicalVolume(ctx context.Context, logicalVolume LogicalVolume) error {
	path := logicalVolume.VolumeGroup.Name + "/" + logicalVolume.Name
	_, err := m.exec(ctx, nil, "lvextend", "-l+100%FREE", path)
	if err != nil {
		if isAlreadyAtSize(err) {
			return nil
		}
		return fmt.Errorf("grow LV %s to consume free space: %w", path, err)
	}
	return nil
}

// isAlreadyAtSize reports whether err is lvextend saying there was nothing to
// grow. It phrases that as a new size not larger than the existing one, with a
// leading New size for a relative target and New size given for an absolute one.
//
// It is a success, not a failure. kubelet reissues NodeExpandVolume after one
// that already succeeded, so a caller taking lvextend at its word fails an
// expansion that had happened, which is the alarming error a redundant expand
// logs today.
//
// Matched on the phrase both variants share, and on that phrase alone: a volume
// group with no room left is a real failure, phrased differently, and a caller
// that swallowed it would report an expansion it never performed.
func isAlreadyAtSize(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not larger than existing size")
}

// ExtendLogicalVolumeToSize grows logicalVolume to an absolute size in bytes
// (lvextend -L<size>B).
func (m *Manager) ExtendLogicalVolumeToSize(ctx context.Context, logicalVolume LogicalVolume, sizeBytes int64) error {
	path := logicalVolume.VolumeGroup.Name + "/" + logicalVolume.Name
	_, err := m.exec(ctx, nil, "lvextend", fmt.Sprintf("-L%dB", sizeBytes), path)
	if err != nil {
		if isAlreadyAtSize(err) {
			return nil
		}
		return fmt.Errorf("grow LV %s to %d bytes: %w", path, sizeBytes, err)
	}
	return nil
}
