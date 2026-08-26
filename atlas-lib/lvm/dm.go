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

// RemoveOrphanedDMNodes clears any live device-mapper nodes whose name starts
// with namePrefix (escaped internally), for when the backing device is gone
// and the higher-level removal (RemoveVolumeGroup, etc.) can no longer read
// the metadata it needs to deactivate cleanly. Retries across a few passes so
// removing a dependent node unblocks what it was blocking, rather than
// hardcoding the dependency chain.
func (m *Manager) RemoveOrphanedDMNodes(ctx context.Context, namePrefix string) error {
	out, err := m.run(ctx, "dmsetup", "ls")
	if err != nil {
		return fmt.Errorf("dmsetup ls: %w", err)
	}

	escaped := escapeDMName(namePrefix)

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
	if len(names) == 0 {
		return nil
	}

	var lastErr error
	for pass := 0; pass < 3 && len(names) > 0; pass++ {
		var remaining []string
		for _, name := range names {
			if _, err := m.run(ctx, "dmsetup", "remove", name); err != nil {
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
