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
	Handles(def LogicalVolumeDefinition) bool
	CreateVolumeArgs(def LogicalVolumeDefinition) []string
}

var volumeProvisioning = map[string]VolumeProvisioning{}

func RegisterVolumeProvisioning(handler VolumeProvisioning) {
	volumeProvisioning[handler.Name()] = handler
}

// CreatePhysicalVolume writes an LVM PV signature onto pv's device (pvcreate),
// scoped to that device, and returns pv unchanged for chaining into
// CreateVolumeGroup.
func (m *Manager) CreatePhysicalVolume(ctx context.Context, pv PhysicalVolume) (PhysicalVolume, error) {
	_, err := m.exec(ctx, []string{pv.DevicePath}, "pvcreate", pv.DevicePath)
	if err != nil {
		return PhysicalVolume{}, fmt.Errorf("pvcreate %s: %w", pv.DevicePath, err)
	}
	return pv, nil
}

// CreateVolumeGroup creates volumeGroup on top of pvs (vgcreate), scoped to
// those devices, and returns volumeGroup unchanged for chaining. pvs is
// variadic so a striped volume group across several members and a
// single-device volume group (VDO's case) share the same call shape.
func (m *Manager) CreateVolumeGroup(
	ctx context.Context, volumeGroup VolumeGroup, pvs ...PhysicalVolume,
) (VolumeGroup, error) {
	paths := devicePaths(pvs)
	args := append([]string{"vgcreate", volumeGroup.Name}, paths...)
	if _, err := m.exec(ctx, paths, args...); err != nil {
		return VolumeGroup{}, fmt.Errorf("vgcreate %s on %v: %w", volumeGroup.Name, paths, err)
	}
	return volumeGroup, nil
}

// ActivateVolumeGroup activates volumeGroup's logical volumes (vgchange -ay),
// never recreating or reformatting anything.
func (m *Manager) ActivateVolumeGroup(ctx context.Context, volumeGroup VolumeGroup) error {
	if _, err := m.exec(ctx, nil, "vgchange", "-ay", volumeGroup.Name); err != nil {
		return fmt.Errorf("activate VG %s: %w", volumeGroup.Name, err)
	}
	return nil
}

// DeactivateVolumeGroup deactivates (but does not destroy) volumeGroup
// (vgchange -an).
func (m *Manager) DeactivateVolumeGroup(ctx context.Context, volumeGroup VolumeGroup) error {
	if _, err := m.exec(ctx, nil, "vgchange", "-an", volumeGroup.Name); err != nil {
		return fmt.Errorf("deactivate VG %s: %w", volumeGroup.Name, err)
	}
	return nil
}

// RemoveVolumeGroup deactivates and removes volumeGroup, destroying its data
// (vgremove -f).
func (m *Manager) RemoveVolumeGroup(ctx context.Context, volumeGroup VolumeGroup) error {
	if _, err := m.exec(ctx, nil, "vgremove", "-f", volumeGroup.Name); err != nil {
		return fmt.Errorf("remove VG %s: %w", volumeGroup.Name, err)
	}
	return nil
}

// CreateLogicalVolume creates a logical volume named logicalVolume in
// volumeGroup, sized to consume the volume group's full free space, targeting
// volumeGroup/poolName (lvcreate), and returns its identity.
//
// poolName is not a physical volume, despite an earlier version of this
// function naming the parameter that way. lvcreate's <vg>/<pool> form names
// the pool a new logical volume is created inside, which only a pool-based
// VolumeProvisioning handler needs: VDO's handler creates both the pool and
// the logical volume in this one lvcreate call, via poolName.
//
// Internally, depending on the LogicalVolumeDefinition, the function may
// delegate parts of the creation process to a VolumeProvisioning handler.
func (m *Manager) CreateLogicalVolume(
	ctx context.Context, volumeGroup VolumeGroup, poolName, logicalVolume string, def LogicalVolumeDefinition,
) (LogicalVolume, error) {
	args := []string{
		"lvcreate",
		"-n", logicalVolume,
		"-l", "100%FREE",
		volumeGroup.Name + "/" + poolName,
		"--yes",
	}

	for _, handler := range volumeProvisioning {
		if !handler.Handles(def) {
			continue
		}
		if additionalArgs := handler.CreateVolumeArgs(def); len(additionalArgs) > 0 {
			args = append(args, additionalArgs...)
		}
		break
	}
	if _, err := m.exec(ctx, nil, args...); err != nil {
		return LogicalVolume{}, fmt.Errorf("lvcreate %s/%s: %w", volumeGroup.Name, logicalVolume, err)
	}
	return LogicalVolume{VolumeGroup: volumeGroup, Name: logicalVolume}, nil
}

// LogicalVolumeSize returns logicalVolume's current size, in bytes.
func (m *Manager) LogicalVolumeSize(ctx context.Context, logicalVolume LogicalVolume) (int64, error) {
	path := logicalVolume.VolumeGroup.Name + "/" + logicalVolume.Name
	out, err := m.exec(ctx, nil, "lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", path)
	if err != nil {
		return 0, fmt.Errorf("read size of %s: %w", path, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size of %s from %q: %w", path, out, err)
	}
	return size, nil
}
