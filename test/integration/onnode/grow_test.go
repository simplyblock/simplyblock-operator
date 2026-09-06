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

// growRecord is where the staged size is left for the phase that comes after,
// outside the volume so that growing the volume cannot disturb it.
func growRecord(name string) string { return filepath.Join("/var/tmp", "volstack-grow-"+name) }

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

// TestGrowStage brings the stack up and leaves it up, because the phase after it
// is a separate run of this binary against the same node.
func TestGrowStage(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets...)
	plan := h.growPlan(t)

	art, err := h.runner().Up(ctx, h.handle(), plan)
	if err != nil {
		t.Fatalf("bring the stack up: %v", err)
	}

	marker := filepath.Join(art.Path, "written-before-the-volume-grew")
	if err := os.WriteFile(marker, []byte("still here"), 0o600); err != nil {
		t.Fatalf("write into the staged filesystem: %v", err)
	}

	staged := filesystemBytes(t, art.Path)
	name := envOr("SB_GROW_FS", "ext4") + "-" + envOr("SB_GROW_PLAN", "lvm")
	if err := os.WriteFile(growRecord(name), []byte(strconv.FormatUint(staged, 10)), 0o600); err != nil {
		t.Fatalf("record the staged size: %v", err)
	}
	t.Logf("staged %s at %s, %d bytes", name, art.Path, staged)
}

// TestGrowExtend runs after the driver has grown the namespaces underneath, and
// is what a NodeExpandVolume does: take the space that is already there.
func TestGrowExtend(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	name := envOr("SB_GROW_FS", "ext4") + "-" + envOr("SB_GROW_PLAN", "lvm")
	staged := readGrowRecord(t, name)
	plan := h.growPlan(t)

	// The namespace grew on the target's side. The initiator learns of it from an
	// asynchronous event, so the device is polled rather than assumed: acting
	// before the kernel has caught up would resize onto the size it had before
	// and report success.
	for _, target := range h.targets {
		awaitLargerNamespace(ctx, t, h, target)
	}

	if err := h.runner().Grow(ctx, plan); err != nil {
		t.Fatalf("grow the stack: %v", err)
	}
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	grown := filesystemBytes(t, h.volume.StagingPath)
	if grown <= staged {
		t.Fatalf("the filesystem is %d bytes and was %d before the volume grew, so nothing took the space",
			grown, staged)
	}

	// The space is worth nothing if the volume did not survive taking it.
	marker := filepath.Join(h.volume.StagingPath, "written-before-the-volume-grew")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("what was written before the volume grew is gone: %v", err)
	}

	if envOr("SB_GROW_PLAN", "lvm") == "striped" {
		assertStillStriped(ctx, t, h)
	}
	t.Logf("%s grew from %d to %d bytes", name, staged, grown)
}

// assertStillStriped checks that the extension went across the same legs.
//
// LVM places new extents on as many members as the existing segment uses, and
// fails when it cannot rather than appending a linear one, so a volume that came
// back with a different stripe count would mean it had fallen back and left half
// the volume spread and half of it not.
func assertStillStriped(ctx context.Context, t *testing.T, h *harness) {
	t.Helper()
	path := h.volume.VolumeGroup() + "/" + h.volume.LogicalVolume()

	out, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "seg_count", path)
	if err != nil {
		t.Fatalf("read the segments of %s: %v", path, err)
	}
	segments := strings.TrimSpace(out)

	stripes, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "stripes", path)
	if err != nil {
		t.Fatalf("read the stripe count of %s: %v", path, err)
	}
	if got, want := strings.TrimSpace(stripes), strconv.Itoa(len(h.targets)); got != want {
		t.Errorf("the grown volume reports %s stripes and was built with %s, across %s segments",
			got, want, segments)
	}
}

// awaitLargerNamespace waits for the initiator to see the capacity the target
// now serves, asking the kernel to look again in case the event was missed.
func awaitLargerNamespace(ctx context.Context, t *testing.T, h *harness, target Target) {
	t.Helper()
	plan := volstack.Plan{h.node.fabric(target)}
	handle := h.volume.UUID + "-grow-" + target.NQN

	art, err := h.runner().Up(ctx, handle, plan)
	if err != nil {
		t.Fatalf("attach %s to look at its size: %v", target.NQN, err)
	}
	dev, ok := art.Device()
	if !ok {
		t.Fatalf("attaching %s exposed %d devices, want one", target.NQN, len(art.Devices))
	}

	_ = runTool(ctx, "nvme", "ns-rescan", controllerOf(dev.Path))

	deadline := time.Now().Add(2 * time.Minute)
	for {
		again, err := blockdev.ResolveDevice(dev.Path)
		if err != nil {
			t.Fatalf("resolve %s: %v", dev.Path, err)
		}
		if again.SizeBytes > dev.SizeBytes {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still %d bytes, so the initiator never saw the namespace grow",
				dev.Path, again.SizeBytes)
		}
		time.Sleep(time.Second)
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

func readGrowRecord(t *testing.T, name string) uint64 {
	t.Helper()
	raw, err := os.ReadFile(growRecord(name)) //nolint:gosec // a path this suite wrote
	if err != nil {
		t.Fatalf("read the size staged by the phase before: %v", err)
	}
	staged, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		t.Fatalf("the recorded size is unreadable: %v", err)
	}
	return staged
}
