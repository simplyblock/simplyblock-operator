//go:build linux

// A volume that grows by gaining members.
//
// The other way capacity arrives, and the one a striped export depends on: the
// members it has cannot grow, so more are attached and the volume spreads onto
// them. Every layer has something different to do about that. The new namespaces
// are connected, labeled, taken into the group, the volume is extended across
// them, and the filesystem is told to use what appeared. Miss the last of those
// and a pod sees nothing at all.
//
// The bring-up comes first and the expand second, because a plan that gained
// members is a different plan: growing alone only observes the layers that
// cannot grow, so nothing would attach the new namespaces or label them, and the
// group would be handed devices carrying nothing.

package onnode

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// requireExtendPhase skips unless a driver is adding members to a volume.
func requireExtendPhase(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_EXTEND_MEMBERS") == "" {
		t.Skip("no member is being added: SB_EXTEND_MEMBERS is unset")
	}
}

// extendPlan is the striped stack over however many members this phase was told
// about, which is the whole point: the second phase is told about more.
func (h *harness) extendPlan() volstack.Plan {
	volume := h.volume
	volume.FsType = envOr("SB_EXTEND_FS", "ext4")
	return h.node.Striped(h.targets, volume, lvm.LogicalVolumeDefinition{
		// The stripe count stays what the volume was built with. LVM places new
		// extents across as many members as the last segment used, so a volume
		// striped two ways spreads onto two of the new members at a time.
		Stripes:          extendStripes(),
		StripeChunkBytes: 64 << 10,
	})
}

// extendStripes is the width the volume was created with, which both phases have
// to agree on or the second describes a different volume than the first made.
func extendStripes() int {
	if n, err := strconv.Atoi(envOr("SB_EXTEND_STRIPES", "2")); err == nil && n > 0 {
		return n
	}
	return 2
}

// TestExtendStage builds the volume over the members it starts with.
func TestExtendStage(t *testing.T) {
	requireExtendPhase(t)
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets...)

	plan := h.extendPlan()
	art, err := h.runner().Up(ctx, h.handle(), plan)
	if err != nil {
		t.Fatalf("bring the stack up: %v", err)
	}

	marker := filepath.Join(art.Path, "written-before-the-members-arrived")
	if err := os.WriteFile(marker, []byte("still here"), 0o600); err != nil {
		t.Fatalf("write into the staged filesystem: %v", err)
	}

	state := growState{FilesystemBytes: filesystemBytes(t, art.Path)}
	writeGrowState(t, state)
	t.Logf("staged over %d members at %s, filesystem %d bytes",
		len(h.targets), art.Path, state.FilesystemBytes)
}

// TestExtendMembers is told about more members than the phase before it, and has
// to make the volume use them.
func TestExtendMembers(t *testing.T) {
	requireExtendPhase(t)
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	staged := readGrowState(t)
	plan := h.extendPlan()

	// Nothing is blanked. The members that were here carry the volume, and the
	// ones that are new carry nothing, which is what the layers are for.
	//
	// The bring-up is what attaches and labels the new members and takes them
	// into the group, and it has to happen before the expand: growing alone
	// observes the layers that cannot grow rather than converging them, so the
	// new namespaces would not be connected and the group would be handed devices
	// with no label on them.
	if _, err := h.runner().Up(ctx, h.handle(), plan); err != nil {
		t.Fatalf("bring the stack up over its new members: %v", err)
	}
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	before := h.segmentCount(ctx, t)

	if err := h.runner().Grow(ctx, plan); err != nil {
		t.Fatalf("grow the stack onto its new members: %v", err)
	}

	// The filesystem is the point. Everything below it can take the space and a
	// pod still sees the size it saw yesterday unless the last layer acts.
	grown := filesystemBytes(t, h.volume.StagingPath)
	if grown <= staged.FilesystemBytes {
		t.Fatalf("the filesystem is %d bytes and was %d before the members arrived, "+
			"so nothing was extended onto them", grown, staged.FilesystemBytes)
	}

	marker := filepath.Join(h.volume.StagingPath, "written-before-the-members-arrived")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("what was written before the members arrived is gone: %v", err)
	}

	// A volume that spread onto the new members has a segment it did not have.
	// Growing in place would have left the count where it was, which is the other
	// way this could have got bigger and is not what is under test.
	after := h.segmentCount(ctx, t)
	if after <= before {
		t.Errorf("the volume has %d segments and had %d, so it did not spread onto the new members",
			after, before)
	}
	t.Logf("the filesystem grew from %d to %d bytes across %d segments",
		staged.FilesystemBytes, grown, after)
}

// segmentCount is how many pieces LVM has laid this volume out in. One per
// allocation, so taking on members adds one.
func (h *harness) segmentCount(ctx context.Context, t *testing.T) int {
	t.Helper()
	path := h.volume.VolumeGroup() + "/" + h.volume.LogicalVolume()
	out, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "seg_count", path)
	if err != nil {
		t.Fatalf("read the segments of %s: %v", path, err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("the segment count of %s is unreadable: %q", path, out)
	}
	return count
}
