package lvm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ExpandPhysicalVolume grows devicePath's PV to its device's current full
// size (pvresize), after the backing device itself has grown.
func (m *Manager) ExpandPhysicalVolume(ctx context.Context, devices []string, devicePath string) error {
	_, err := m.Run(ctx, devices, "pvresize", devicePath)
	if err != nil {
		return fmt.Errorf("pvresize %s: %w", devicePath, err)
	}
	return nil
}

// ExtendVolumeGroup adds devicePaths to vg (vgextend), scoped to devices. The
// counterpart to CreateVolumeGroup for a VG that already exists: growing a
// striped volume group by adding members, as opposed to ExpandPhysicalVolume,
// which grows a PV already in the VG to its device's current full size.
func (m *Manager) ExtendVolumeGroup(ctx context.Context, devices []string, vg string, devicePaths ...string) error {
	args := append([]string{"vgextend", vg}, devicePaths...)
	if _, err := m.Run(ctx, devices, args...); err != nil {
		return fmt.Errorf("vgextend %s with %v: %w", vg, devicePaths, err)
	}
	return nil
}

// ExpandLogicalVolume grows vg's lvName logical volume to consume all newly
// available free space in the VG (lvextend -l+100%FREE). Always the full
// free space: unlike ExtendLogicalVolumeToSize, there is no partial-expand
// use case for this package's callers.
//
// The "+" prefix matters: lvextend's -l (unlike lvcreate's) treats a bare
// "100%FREE" as an ABSOLUTE target size (100% of what's currently free), not
// "grow by." Confirmed live: "New size given (1024 extents) not larger than
// existing size (1535 extents)," since free-space-alone is smaller than the
// volume's current size. "+100%FREE" is additive (current size + free
// space), which is what "grow to consume all newly available space" means.
func (m *Manager) ExpandLogicalVolume(ctx context.Context, devices []string, vg, lvName string) error {
	_, err := m.Run(ctx, devices, "lvextend", "-l+100%FREE", vg+"/"+lvName)
	if err != nil {
		return fmt.Errorf("grow LV %s/%s to consume free space: %w", vg, lvName, err)
	}
	return nil
}

// LogicalVolumeSize returns vg's lvName logical volume's current size, in
// bytes.
func (m *Manager) LogicalVolumeSize(ctx context.Context, devices []string, vg, lvName string) (int64, error) {
	out, err := m.Run(ctx, devices, "lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", vg+"/"+lvName)
	if err != nil {
		return 0, fmt.Errorf("read size of %s/%s: %w", vg, lvName, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size of %s/%s from %q: %w", vg, lvName, out, err)
	}
	return size, nil
}

// ExtendLogicalVolumeToSize grows vg's lvName logical volume to an absolute
// size in bytes (lvextend -L<size>B).
func (m *Manager) ExtendLogicalVolumeToSize(
	ctx context.Context, devices []string, vg, lvName string, sizeBytes int64,
) error {
	_, err := m.Run(ctx, devices, "lvextend", fmt.Sprintf("-L%dB", sizeBytes), vg+"/"+lvName)
	if err != nil {
		return fmt.Errorf("grow LV %s/%s to %d bytes: %w", vg, lvName, sizeBytes, err)
	}
	return nil
}
