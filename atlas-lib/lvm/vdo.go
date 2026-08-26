package lvm

import (
	"context"
	"fmt"
)

// CreateVDOLogicalVolume creates a VDO-backed logical volume named lvName
// inside vg's pool poolName, sized to consume the pool's full capacity, with
// compression and deduplication set independently at creation time. Changing
// them on an already-existing volume is SetVDOFeatures' job, not this
// method's.
func (m *Manager) CreateVDOLogicalVolume(
	ctx context.Context, devices []string, vg, poolName, lvName string, compression, deduplication bool,
) error {
	_, err := m.Run(ctx, devices,
		"lvcreate",
		"--type", "vdo",
		"--config", "activation{checks=0}",
		"-n", lvName,
		"-l", "100%FREE",
		"--compression", yn(compression),
		"--deduplication", yn(deduplication),
		vg+"/"+poolName,
		"--yes",
	)
	if err != nil {
		return fmt.Errorf("lvcreate --type vdo for %s: %w", vg, err)
	}
	return nil
}

// SetVDOFeatures toggles compression/deduplication on an existing, active VDO
// pool without recreating it (lvchange). Pass nil devices to address vg by
// name alone.
func (m *Manager) SetVDOFeatures(
	ctx context.Context, devices []string, vg, poolName string, compression, deduplication bool,
) error {
	_, err := m.Run(ctx, devices,
		"lvchange", "--compression", yn(compression), "--deduplication", yn(deduplication), vg+"/"+poolName,
	)
	if err != nil {
		return fmt.Errorf("set VDO features on %s/%s: %w", vg, poolName, err)
	}
	return nil
}

func yn(b bool) string {
	if b {
		return "y"
	}
	return "n"
}
