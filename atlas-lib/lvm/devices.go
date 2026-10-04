package lvm

import (
	"context"
	"fmt"
)

// ForgetDevice removes path from the node's LVM devices file
// (/etc/lvm/devices/system.devices), which restricts LVM's default device
// visibility. It is hygiene rather than a correctness mechanism: nothing
// prunes this file on its own, so a node accumulates one stale entry per
// device that has gone without a clean unstage, unbounded over the node's
// lifetime.
//
// Scoped to path the same way CreatePhysicalVolume and RemovePhysicalVolume
// are, because lvmdevices --deldev takes exactly the device it is told to
// forget and nothing wider.
// Convergent, like every other removal here: a device the file does not list is
// already in the state the caller asked for. That is the common answer, since
// nothing adds an entry for a device that was never labeled.
func (m *Manager) ForgetDevice(ctx context.Context, path string) error {
	if _, err := m.exec(ctx, []string{path}, "lvmdevices", "--deldev", path); err != nil {
		if isAlreadyGone(err) {
			return nil
		}
		return fmt.Errorf("lvmdevices --deldev %s: %w", path, err)
	}
	return nil
}
