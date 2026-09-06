//go:build linux

// What a plan does about a volume that is not what it says it is.
//
// A volume formatted as one filesystem and asked for as another is somebody
// having changed what a class says about a volume that already exists. There is
// no safe way to reconcile that: reformatting destroys it, and serving what is
// there leaves a volume nobody declared, until whatever notices next decides to
// make the device match the class. That decision reformats.
//
// So every case here asserts two things, and the second is the one that matters:
// the bring-up refuses, and the volume is untouched afterward. A refusal that
// wiped the device on its way out would satisfy the first alone.

package onnode

import (
	"context"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
)

// onDevice attaches one namespace, hands the device over, and detaches it again.
// It is how a case reaches the bytes underneath a plan without going through the
// layers that are under test.
func (h *harness) onDevice(ctx context.Context, target Target, do func(dev blockdev.Device)) {
	h.t.Helper()
	plan := volstack.Plan{h.node.fabric(target)}
	handle := h.volume.UUID + "-direct"

	art, err := h.runner().Up(ctx, handle, plan)
	if err != nil {
		h.t.Fatalf("attach %s: %v", target.NQN, err)
	}
	defer func() {
		if err := h.runner().Down(context.WithoutCancel(ctx), handle, plan); err != nil {
			h.t.Errorf("detach %s: %v", target.NQN, err)
		}
	}()

	dev, ok := art.Device()
	if !ok {
		h.t.Fatalf("attaching %s exposed %d devices, want one", target.NQN, len(art.Devices))
	}
	do(dev)
}

// carries is what the volume reads as, through the same reading a plan uses.
func (h *harness) carries(ctx context.Context, target Target) blockdev.Reading {
	h.t.Helper()
	var reading blockdev.Reading
	h.onDevice(ctx, target, func(dev blockdev.Device) {
		got, err := h.node.content.Read(ctx, dev)
		if err != nil {
			h.t.Fatalf("read what %s carries: %v", dev.Path, err)
		}
		reading = got
	})
	return reading
}

// formatAs puts a filesystem on the volume by bringing a plan up for it, so the
// volume is left exactly as a plan for that filesystem leaves one.
func (h *harness) formatAs(ctx context.Context, target Target, fsType string) {
	h.t.Helper()
	volume := h.volume
	volume.FsType = fsType

	plan := volstack.Plan{h.node.fabric(target), h.node.filesystem(volume)}
	handle := h.volume.UUID + "-setup-" + fsType
	if _, err := h.runner().Up(ctx, handle, plan); err != nil {
		h.t.Fatalf("format %s as %s: %v", target.NQN, fsType, err)
	}
	if err := h.runner().Down(ctx, handle, plan); err != nil {
		h.t.Fatalf("unstage after formatting as %s: %v", fsType, err)
	}
}

// A volume carrying one filesystem and a plan asking for another.
func TestPlainStackRefusesAFilesystemTheClassDidNotAskFor(t *testing.T) {
	for _, tc := range []struct {
		onDisk, asks string
	}{
		{onDisk: "ext4", asks: "xfs"},
		{onDisk: "xfs", asks: "ext4"},
	} {
		t.Run(tc.onDisk+" asked for as "+tc.asks, func(t *testing.T) {
			h := newHarness(t)
			ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
			defer cancel()

			h.blank(ctx, h.targets[0])
			h.formatAs(ctx, h.targets[0], tc.onDisk)

			volume := h.volume
			volume.FsType = tc.asks
			plan := volstack.Plan{h.node.fabric(h.targets[0]), h.node.filesystem(volume)}

			_, err := h.runner().Up(ctx, h.handle(), plan)
			if err == nil {
				h.down(ctx, plan)
				t.Fatalf("a volume carrying %s staged for a class asking for %s", tc.onDisk, tc.asks)
			}
			for _, want := range []string{tc.onDisk, tc.asks} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %s: %v", want, err)
				}
			}

			// The half that would be catastrophic to get wrong.
			after := h.carries(ctx, h.targets[0])
			if after.Content != blockdev.ContentFilesystem || after.Type != tc.onDisk {
				t.Fatalf("the volume now reads as %s %q, and it carried %s before the refusal",
					after.Content, after.Type, tc.onDisk)
			}
		})
	}
}

// gptHeader is what a partition table looks like to the reading: the signature
// the specification puts at the second block. Written by hand because the point
// is a volume carrying something that is not a filesystem at all, and nothing
// here needs it to be a partition table anybody could use.
var gptHeader = []byte("EFI PART")

// A volume carrying something that is not a filesystem, asked for as one. The
// layer has no filesystem to disagree with here: it refuses because the volume
// is somebody's and it cannot tell whose.
func TestPlainStackRefusesAVolumeCarryingSomethingElse(t *testing.T) {
	for _, asks := range []string{"ext4", "xfs"} {
		t.Run("asked for as "+asks, func(t *testing.T) {
			h := newHarness(t)
			ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
			defer cancel()

			h.blank(ctx, h.targets[0])
			h.onDevice(ctx, h.targets[0], func(dev blockdev.Device) {
				writeAt(t, dev, int64(dev.LogicalBlockSize), gptHeader)
			})

			before := h.carries(ctx, h.targets[0])
			if before.Content == blockdev.ContentBlank {
				t.Fatalf("the fixture did not take: the volume still reads blank")
			}

			volume := h.volume
			volume.FsType = asks
			plan := volstack.Plan{h.node.fabric(h.targets[0]), h.node.filesystem(volume)}

			if _, err := h.runner().Up(ctx, h.handle(), plan); err == nil {
				h.down(ctx, plan)
				t.Fatalf("a volume carrying %s staged for a class asking for %s", before.Type, asks)
			}

			after := h.carries(ctx, h.targets[0])
			if after.Content != before.Content || after.Type != before.Type {
				t.Fatalf("the volume read as %s %q before the refusal and %s %q after",
					before.Content, before.Type, after.Content, after.Type)
			}
		})
	}
}
