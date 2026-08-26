package lvm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type LogicalVolumeDefinition struct {
	Deduplication bool
	Compression   bool
}

type VolumeProvisioning interface {
	Name() string
	CreateVolumeArgs(def LogicalVolumeDefinition) []string
}

var volumeProvisioning = map[string]VolumeProvisioning{}

func RegisterVolumeProvisioning(handler VolumeProvisioning) {
	volumeProvisioning[handler.Name()] = handler
}

// CreatePhysicalVolume writes an LVM PV signature onto devicePath
// (pvcreate), scoped to that device.
func (m *Manager) CreatePhysicalVolume(ctx context.Context, devicePath string) error {
	_, err := m.exec(ctx, []string{devicePath}, "pvcreate", devicePath)
	if err != nil {
		return fmt.Errorf("pvcreate %s: %w", devicePath, err)
	}
	return nil
}

// CreateVolumeGroup creates volumeGroup on top of devicePaths (vgcreate),
// scoped to those devices. devicePaths is variadic so a striped volume group
// across several members and a single-device volume group (VDO's case) share
// the same call shape.
func (m *Manager) CreateVolumeGroup(
	ctx context.Context, volumeGroup string, devicePaths ...string,
) error {
	args := append([]string{"vgcreate", volumeGroup}, devicePaths...)
	if _, err := m.exec(ctx, devicePaths, args...); err != nil {
		return fmt.Errorf("vgcreate %s on %v: %w", volumeGroup, devicePaths, err)
	}
	return nil
}

// ActivateVolumeGroup activates volumeGroup's logical volumes (vgchange -ay),
// never recreating or reformatting anything.
func (m *Manager) ActivateVolumeGroup(ctx context.Context, volumeGroup string) error {
	if _, err := m.exec(ctx, nil, "vgchange", "-ay", volumeGroup); err != nil {
		return fmt.Errorf("activate VG %s: %w", volumeGroup, err)
	}
	return nil
}

// DeactivateVolumeGroup deactivates (but does not destroy) volumeGroup
// (vgchange -an).
func (m *Manager) DeactivateVolumeGroup(ctx context.Context, volumeGroup string) error {
	if _, err := m.exec(ctx, nil, "vgchange", "-an", volumeGroup); err != nil {
		return fmt.Errorf("deactivate VG %s: %w", volumeGroup, err)
	}
	return nil
}

// RemoveVolumeGroup deactivates and removes volumeGroup, destroying its data
// (vgremove -f).
func (m *Manager) RemoveVolumeGroup(ctx context.Context, volumeGroup string) error {
	if _, err := m.exec(ctx, nil, "vgremove", "-f", volumeGroup); err != nil {
		return fmt.Errorf("remove VG %s: %w", volumeGroup, err)
	}
	return nil
}

// CreateLogicalVolume creates the logical volume logicalVolume on
// volumeGroup's physicalVolume (lvcreate), sized to consume the volume group's
// full free space.
//
// Internally, depending on the LogicalVolumeDefinition, the function may
// delegate parts of the creation process to a VolumeProvisioning handler.
func (m *Manager) CreateLogicalVolume(
	ctx context.Context, volumeGroup, physicalVolume, logicalVolume string, def LogicalVolumeDefinition,
) error {
	args := []string{
		"lvcreate",
		"-n", logicalVolume,
		"-l", "100%FREE",
		volumeGroup + "/" + physicalVolume,
		"--yes",
	}

	if handler, ok := volumeProvisioning["vdo"]; ok {
		if additionalArgs := handler.CreateVolumeArgs(def); len(additionalArgs) > 0 {
			args = append(args, additionalArgs...)
		}
	}
	if _, err := m.exec(ctx, nil, args...); err != nil {
		return fmt.Errorf("lvcreate %s/%s: %w", volumeGroup, logicalVolume, err)
	}
	return nil
}

// LogicalVolumeSize returns the current size of volumeGroup's
// logicalVolume logical volume, in bytes.
func (m *Manager) LogicalVolumeSize(
	ctx context.Context, volumeGroup, logicalVolume string,
) (int64, error) {
	out, err := m.exec(ctx, nil,
		"lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size",
		volumeGroup+"/"+logicalVolume,
	)
	if err != nil {
		return 0, fmt.Errorf("read size of %s/%s: %w", volumeGroup, logicalVolume, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size of %s/%s from %q: %w", volumeGroup, logicalVolume, out, err)
	}
	return size, nil
}
