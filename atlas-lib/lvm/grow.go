package lvm

import (
	"context"
	"fmt"
)

// ExpandPhysicalVolume grows devicePath's PV to its device's current full
// size (pvresize), after the backing device itself has grown.
//
// Unscoped although it names a device, unlike CreatePhysicalVolume: pvresize
// rewrites the metadata of the volume group the PV belongs to, which lives on
// every member of that group, so a scope narrowed to this one device would
// hide the rest of the group from a command that has to write to it.
func (m *Manager) ExpandPhysicalVolume(ctx context.Context, devicePath string) error {
	_, err := m.exec(ctx, nil, "pvresize", devicePath)
	if err != nil {
		return fmt.Errorf("pvresize %s: %w", devicePath, err)
	}
	return nil
}

// ExtendVolumeGroup adds devicePaths to volumeGroup (vgextend). The
// counterpart to CreateVolumeGroup for a volume group that already exists:
// growing a striped volume group by adding members, as opposed to
// ExpandPhysicalVolume, which grows a PV already in the volume group to its
// device's current full size.
//
// Unscoped, for the reason ExpandPhysicalVolume gives: the command has to
// resolve the existing volume group, whose current members are not among
// devicePaths.
func (m *Manager) ExtendVolumeGroup(
	ctx context.Context, volumeGroup string, devicePaths ...string,
) error {
	args := append([]string{"vgextend", volumeGroup}, devicePaths...)
	if _, err := m.exec(ctx, nil, args...); err != nil {
		return fmt.Errorf("vgextend %s with %v: %w", volumeGroup, devicePaths, err)
	}
	return nil
}

// ExpandLogicalVolume grows volumeGroup's logicalVolume to consume all newly
// available free space in the volume group (lvextend -l+100%FREE). Always the
// full free space: unlike ExtendLogicalVolumeToSize, there is no
// partial-expand use case for this package's callers.
//
// The "+" prefix matters: lvextend's -l (unlike lvcreate's) treats a bare
// "100%FREE" as an ABSOLUTE target size (100% of what's currently free), not
// "grow by." Confirmed live: "New size given (1024 extents) not larger than
// existing size (1535 extents)," since free-space-alone is smaller than the
// volume's current size. "+100%FREE" is additive (current size + free
// space), which is what "grow to consume all newly available space" means.
func (m *Manager) ExpandLogicalVolume(ctx context.Context, volumeGroup, logicalVolume string) error {
	_, err := m.exec(ctx, nil, "lvextend", "-l+100%FREE", volumeGroup+"/"+logicalVolume)
	if err != nil {
		return fmt.Errorf("grow LV %s/%s to consume free space: %w", volumeGroup, logicalVolume, err)
	}
	return nil
}

// ExtendLogicalVolumeToSize grows volumeGroup's logicalVolume to an absolute
// size in bytes (lvextend -L<size>B).
func (m *Manager) ExtendLogicalVolumeToSize(
	ctx context.Context, volumeGroup, logicalVolume string, sizeBytes int64,
) error {
	_, err := m.exec(ctx, nil,
		"lvextend", fmt.Sprintf("-L%dB", sizeBytes), volumeGroup+"/"+logicalVolume,
	)
	if err != nil {
		return fmt.Errorf("grow LV %s/%s to %d bytes: %w", volumeGroup, logicalVolume, sizeBytes, err)
	}
	return nil
}
