//go:build linux

// Growing a volume, all the way up.
//
// The whole chain has to move for a pod to get the space: the file behind the
// namespace, the loop device holding it, nvmet's idea of the capacity, the
// initiator's namespace, the physical volumes, the logical volume, and finally
// the filesystem. Everything below the initiator belongs to whoever serves the
// volume, so the driver grows that side and this side is asked to catch up,
// which is exactly the division a real expand has.
//
// That is why these run in two phases with the driver acting in between. A case
// that grew the backing store itself would be proving that a test can resize a
// loop device.

package onnode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack"
)

// growState is what the staging phase leaves for the phase that extends.
//
// The sizes have to be measured before the volume grows, because afterward
// there is nothing to compare against: a phase that read them itself would be
// asking whether the device is the size it just found it to be.
type growState struct {
	// FilesystemBytes is how big the filesystem said it was once staged.
	FilesystemBytes uint64 `json:"filesystemBytes"`

	// DeviceBytes is each member's capacity, in the order the plan uses them.
	DeviceBytes []uint64 `json:"deviceBytes"`
}

// growStatePath is outside the volume, so that growing the volume cannot disturb
// what was recorded about it.
func growStatePath() string {
	return filepath.Join(envOr("SB_RECORDS", "/var/tmp"), "grow-state.json")
}

// growPlan builds the stack under test from what the driver asked for. The LVM
// plans are the point: a filesystem alone grows onto a namespace, and everything
// interesting about an expand is in the two layers between.
func (h *harness) growPlan(t *testing.T) volstack.Plan {
	t.Helper()
	volume := h.volume
	volume.FsType = envOr("SB_GROW_FS", "ext4")

	switch shape := envOr("SB_GROW_PLAN", "lvm"); shape {
	case "plain":
		return h.node.Plain(h.targets[0], volume)
	case "lvm":
		return h.node.LVM(h.targets[0], volume, lvm.LogicalVolumeDefinition{}, "")
	case "striped":
		if len(h.targets) < 2 {
			t.Skip("a striped plan needs a second namespace, and SB_TARGET2_NQN is unset")
		}
		return h.node.Striped(h.targets, volume, lvm.LogicalVolumeDefinition{
			Stripes: len(h.targets), StripeChunkBytes: 64 << 10,
		})
	default:
		t.Fatalf("SB_GROW_PLAN is %q, which is not a plan this suite builds", shape)
		return nil
	}
}

// requireGrowPhase skips unless a driver is running the expand phases.
//
// These two are halves of one case and leave the stack up between them, so
// running either on its own puts a volume group and a mount on a namespace that
// whatever runs next is about to use for something else. A caller that runs this
// binary without naming a test gets everything in it, and that is how these came
// to contaminate a suite that had not asked for them.
func requireGrowPhase(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_GROW_PLAN") == "" {
		t.Skip("no expand is being driven: SB_GROW_PLAN is unset, and these phases leave a stack up")
	}
}

// TestGrowStage brings the stack up and leaves it up, because the phase after it
// is a separate run of this binary against the same node.
func TestGrowStage(t *testing.T) {
	requireGrowPhase(t)
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets...)

	// Measured before the stack is raised, while the members are still plain
	// devices, and kept for the phase that has to tell whether they grew.
	state := growState{DeviceBytes: h.memberSizes(ctx, t)}

	plan := h.growPlan(t)
	art, err := h.runner().Up(ctx, h.handle(), plan)
	if err != nil {
		t.Fatalf("bring the stack up: %v", err)
	}

	marker := filepath.Join(art.Path, "written-before-the-volume-grew")
	if err := os.WriteFile(marker, []byte("still here"), 0o600); err != nil {
		t.Fatalf("write into the staged filesystem: %v", err)
	}

	state.FilesystemBytes = filesystemBytes(t, art.Path)
	writeGrowState(t, state)
	t.Logf("staged at %s: filesystem %d bytes over members %v",
		art.Path, state.FilesystemBytes, state.DeviceBytes)
}

// TestGrowExtend runs after the driver has grown the namespaces underneath, and
// is what a NodeExpandVolume does: take the space that is already there.
func TestGrowExtend(t *testing.T) {
	requireGrowPhase(t)
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	staged := readGrowState(t)
	plan := h.growPlan(t)

	// The namespaces grew on the target's side. The initiator learns of it from
	// an asynchronous event, so each member is waited for against the size it was
	// before rather than against whatever it reads now: acting early would resize
	// onto the size it already had and report success.
	h.awaitLargerMembers(ctx, t, staged.DeviceBytes, grownMembers(len(h.targets)))

	if err := h.runner().Grow(ctx, plan); err != nil {
		t.Fatalf("grow the stack: %v", err)
	}
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	grown := filesystemBytes(t, h.volume.StagingPath)
	if grown <= staged.FilesystemBytes {
		t.Fatalf("the filesystem is %d bytes and was %d before the volume grew, so nothing took the space",
			grown, staged.FilesystemBytes)
	}

	// The space is worth nothing if the volume did not survive taking it.
	marker := filepath.Join(h.volume.StagingPath, "written-before-the-volume-grew")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("what was written before the volume grew is gone: %v", err)
	}

	if envOr("SB_GROW_PLAN", "lvm") == "striped" {
		h.assertStillStriped(ctx, t)
	}
	t.Logf("the filesystem grew from %d to %d bytes", staged.FilesystemBytes, grown)
}

// memberSizes is each member's capacity now, in plan order.
func (h *harness) memberSizes(ctx context.Context, t *testing.T) []uint64 {
	t.Helper()
	sizes := make([]uint64, 0, len(h.targets))
	for _, target := range h.targets {
		h.onDevice(ctx, target, func(dev blockdev.Device) {
			sizes = append(sizes, dev.SizeBytes)
		})
	}
	return sizes
}

// awaitLargerMembers waits until every member reports more capacity than it had
// when the stack was staged, asking the kernel to look again in case the event
// announcing it was missed.
func (h *harness) awaitLargerMembers(ctx context.Context, t *testing.T, was []uint64, grown int) {
	t.Helper()
	if len(was) != len(h.targets) {
		t.Fatalf("the staging phase recorded %d members and this plan has %d", len(was), len(h.targets))
	}

	for i, target := range h.targets {
		if i >= grown {
			// Left at the size it was, deliberately. Waiting for it would time
			// out, and a case that expects the extension to be refused would then
			// pass on the timeout instead of on the refusal.
			continue
		}
		h.onDevice(ctx, target, func(dev blockdev.Device) {
			_ = runTool(ctx, "nvme", "ns-rescan", controllerOf(dev.Path))

			deadline := time.Now().Add(2 * time.Minute)
			for {
				again, err := blockdev.ResolveDevice(dev.Path)
				if err != nil {
					t.Fatalf("resolve %s: %v", dev.Path, err)
				}
				if again.SizeBytes > was[i] {
					t.Logf("%s grew from %d to %d bytes", dev.Path, was[i], again.SizeBytes)
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s is %d bytes and was %d when the stack was staged, "+
						"so the initiator never saw the namespace grow",
						dev.Path, again.SizeBytes, was[i])
				}
				time.Sleep(time.Second)
			}
		})
	}
}

// grownMembers is how many of the members the driver grew, which is all of them
// unless a case is about what happens when it is not.
func grownMembers(members int) int {
	raw := os.Getenv("SB_GROW_MEMBERS")
	if raw == "" {
		return members
	}
	grown, err := strconv.Atoi(raw)
	if err != nil || grown < 0 || grown > members {
		return members
	}
	return grown
}

// assertStillStriped checks that the extension went across the same legs.
//
// LVM places new extents on as many members as the existing segment uses, and
// fails when it cannot rather than appending a linear one, so a volume that came
// back with a different stripe count would mean it had fallen back and left half
// the volume spread and half of it not.
func (h *harness) assertStillStriped(ctx context.Context, t *testing.T) {
	t.Helper()
	path := h.volume.VolumeGroup() + "/" + h.volume.LogicalVolume()

	segments, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "seg_count", path)
	if err != nil {
		t.Fatalf("read the segments of %s: %v", path, err)
	}
	stripes, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "stripes", path)
	if err != nil {
		t.Fatalf("read the stripe count of %s: %v", path, err)
	}
	if got, want := strings.TrimSpace(stripes), strconv.Itoa(len(h.targets)); got != want {
		t.Errorf("the grown volume reports %s stripes and was built with %s, across %s segments",
			got, want, strings.TrimSpace(segments))
	}
}

// controllerOf is the controller a namespace hangs off, which is what a rescan
// is addressed to: /dev/nvme0n1 is served by /dev/nvme0.
func controllerOf(namespace string) string {
	if i := strings.LastIndex(namespace, "n"); i > len("/dev/nvme") {
		return namespace[:i]
	}
	return namespace
}

// filesystemBytes is how big the filesystem at path says it is, which is the
// number a pod is limited by and the one an expand exists to change.
func filesystemBytes(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		t.Fatalf("stat the filesystem at %s: %v", path, err)
	}
	//nolint:gosec // a block count and a block size, neither negative
	return st.Blocks * uint64(st.Bsize)
}

func writeGrowState(t *testing.T, state growState) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(growStatePath()), 0o750); err != nil {
		t.Fatalf("make room for the staged sizes: %v", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode the staged sizes: %v", err)
	}
	if err := os.WriteFile(growStatePath(), raw, 0o600); err != nil {
		t.Fatalf("record the staged sizes: %v", err)
	}
}

func readGrowState(t *testing.T) growState {
	t.Helper()
	raw, err := os.ReadFile(growStatePath()) //nolint:gosec // a path this suite wrote
	if err != nil {
		t.Fatalf("read what the staging phase recorded: %v", err)
	}
	var state growState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("the recorded sizes are unreadable: %v", err)
	}
	return state
}
