package lvm

import (
	"context"
	"fmt"
	"strings"
)

// escapeDMName escapes name the way device-mapper flattens a compound name
// (doubling every literal "-"), so matching dmsetup's own output (e.g.,
// "<vg>-<lv>") against a known VG or LV name compares correctly. Confirmed
// live: matching against an unescaped name found nothing in `dmsetup ls`
// output, leaving an orphaned stack stuck with nothing left to clean it up.
//
// Unexported: RemoveOrphanedDMNodes is the only caller a device-mapper name
// match needs. Nothing outside this package parses dmsetup output directly.
func escapeDMName(name string) string {
	return strings.ReplaceAll(name, "-", "--")
}

// matchingDMNodes lists the live device-mapper nodes whose name starts with
// volumeGroup's (escaped internally), from a plain `dmsetup ls` naming every
// node on the host. It is content the same way RemoveOrphanedDMNodes and
// HasOrphanedDMNodes both need it: the check and the removal must agree on
// which nodes belong to this group, so both go through this one listing
// rather than each parsing dmsetup output on their own.
func (m *Manager) matchingDMNodes(ctx context.Context, volumeGroup VolumeGroup) ([]string, error) {
	out, err := m.exec(ctx, nil, "dmsetup", "ls")
	if err != nil {
		return nil, fmt.Errorf("dmsetup ls: %w", err)
	}

	escaped := escapeDMName(volumeGroup.Name)

	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "No devices found" {
			continue
		}
		name := strings.Fields(line)[0]
		if strings.HasPrefix(name, escaped+"-") {
			names = append(names, name)
		}
	}
	return names, nil
}

// HasOrphanedDMNodes reports whether volumeGroup still has live
// device-mapper nodes mapped on this host, independently of whether any of
// its member devices can currently be read at all.
//
// This is what a layer whose members have vanished entirely — total NVMe-oF
// path loss, rather than an interrupted bring-up — checks in order to answer
// whether it is still holding something this host has to release: content-
// based identity (VolumeGroup, HasLogicalVolume) needs a device to read, and
// there is none left to read when every member is gone. The device-mapper
// nodes RemoveOrphanedDMNodes already knows how to find and remove are the
// one thing that survives the member devices disappearing, so this answers
// the same question by simply not removing what it finds.
func (m *Manager) HasOrphanedDMNodes(ctx context.Context, volumeGroup VolumeGroup) (bool, error) {
	names, err := m.matchingDMNodes(ctx, volumeGroup)
	if err != nil {
		return false, err
	}
	return len(names) > 0, nil
}

// RemoveOrphanedDMNodes clears any live device-mapper nodes whose name starts
// with volumeGroup's (escaped internally), for when the backing device is
// gone and the higher-level removal (RemoveVolumeGroup, etc.) can no longer
// read the metadata it needs to deactivate cleanly. Retries across a few
// passes so removing a dependent node unblocks what it was blocking, rather
// than hardcoding the dependency chain.
func (m *Manager) RemoveOrphanedDMNodes(ctx context.Context, volumeGroup VolumeGroup) error {
	names, err := m.matchingDMNodes(ctx, volumeGroup)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}

	var lastErr error
	for pass := 0; pass < 3 && len(names) > 0; pass++ {
		var remaining []string
		for _, name := range names {
			if _, err := m.exec(ctx, nil, "dmsetup", "remove", name); err != nil {
				remaining = append(remaining, name)
				lastErr = err
			}
		}
		names = remaining
	}
	if len(names) > 0 {
		return fmt.Errorf("failed to remove orphaned dm nodes %v: %w", names, lastErr)
	}
	return nil
}
