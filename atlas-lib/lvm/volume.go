package lvm

import (
	"context"
	"fmt"
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

// CreatePhysicalVolume writes an LVM PV signature onto devicePath (pvcreate),
// scoped to devices.
func (m *Manager) CreatePhysicalVolume(ctx context.Context, devices []string, devicePath string) error {
	_, err := m.Run(ctx, devices, "pvcreate", devicePath)
	if err != nil {
		return fmt.Errorf("pvcreate %s: %w", devicePath, err)
	}
	return nil
}

// CreateVolumeGroup creates vg on top of devicePaths (vgcreate), scoped to
// devices. devicePaths is variadic so a striped volume group across several
// members and a single-device VG (VDO's case) share the same call shape.
func (m *Manager) CreateVolumeGroup(ctx context.Context, devices []string, vg string, devicePaths ...string) error {
	args := append([]string{"vgcreate", vg}, devicePaths...)
	if _, err := m.Run(ctx, devices, args...); err != nil {
		return fmt.Errorf("vgcreate %s on %v: %w", vg, devicePaths, err)
	}
	return nil
}

// ActivateVolumeGroup activates vg's logical volumes (vgchange -ay), never
// recreating or reformatting anything. Pass nil devices to address vg by name
// alone, for a teardown/reattach path with no device path left to scope to.
func (m *Manager) ActivateVolumeGroup(ctx context.Context, devices []string, vg string) error {
	if _, err := m.Run(ctx, devices, "vgchange", "-ay", vg); err != nil {
		return fmt.Errorf("activate VG %s: %w", vg, err)
	}
	return nil
}

// DeactivateVolumeGroup deactivates (but does not destroy) vg (vgchange -an).
// Pass nil devices to address vg by name alone.
func (m *Manager) DeactivateVolumeGroup(ctx context.Context, devices []string, vg string) error {
	if _, err := m.Run(ctx, devices, "vgchange", "-an", vg); err != nil {
		return fmt.Errorf("deactivate VG %s: %w", vg, err)
	}
	return nil
}

// RemoveVolumeGroup deactivates and removes vg, destroying its data
// (vgremove -f). Pass nil devices to address vg by name alone.
func (m *Manager) RemoveVolumeGroup(ctx context.Context, devices []string, vg string) error {
	if _, err := m.Run(ctx, devices, "vgremove", "-f", vg); err != nil {
		return fmt.Errorf("remove VG %s: %w", vg, err)
	}
	return nil
}

func (m *Manager) CreateLogicalVolume(ctx context.Context, devices []string, vg, pv, lvName string, def LogicalVolumeDefinition) error {
	args := []string{
		"lvcreate",
		"-n", lvName,
		"-l", "100%FREE",
		vg + "/" + pv,
		"--yes",
	}

	if handler, ok := volumeProvisioning["vdo"]; ok {
		if additionalArgs := handler.CreateVolumeArgs(def); len(additionalArgs) > 0 {
			args = append(args, additionalArgs...)
		}
	}
	_, err := m.Run(ctx, devices, args...)
	return err
}
