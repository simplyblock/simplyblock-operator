package lvm

import (
	"context"
	"fmt"
)

// ForgetDevice removes pv's entry from this host's LVM devices file
// (/etc/lvm/devices/system.devices), the file the package doc comment
// describes as restricting LVM's default visibility to specific devices.
// Nothing else prunes an entry once its device is gone, so a node that loses
// devices without a clean pvremove accumulates one stale entry per failure
// cycle over its lifetime. The file gates which devices LVM considers rather
// than causing a failure of its own, so a stale entry is hygiene rather than
// a correctness problem: a caller on a teardown path this must not block is
// expected to treat a failure here as best-effort rather than propagate it.
func (m *Manager) ForgetDevice(ctx context.Context, pv PhysicalVolume) error {
	_, err := m.exec(ctx, []string{pv.DevicePath}, "lvmdevices", "--deldev", pv.DevicePath)
	if err != nil {
		return fmt.Errorf("lvmdevices --deldev %s: %w", pv.DevicePath, err)
	}
	return nil
}
