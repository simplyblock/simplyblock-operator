//go:build linux

// How much of its namespaces an LVM stack gives the volume on top, measured on a
// real kernel with the LVM the driver ships.
//
// The CSI controller sizes the control-plane volumes under an LVM stack so that
// the device on top holds what the claim asked for, and it can only do that by
// knowing what LVM keeps for itself (lvm.PhysicalVolumeOverheadBytes). That is
// LVM's defaults, not the driver's code, so it is measured here rather than
// assumed. VDO keeps far more, for its own metadata and index, and how much is
// what the VDO case records.

package onnode

import (
	"context"
	"testing"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack/plans"
)

// namespaceBytes is the size of each target's namespace, read over a raw
// attachment of it.
func (h *harness) namespaceBytes(ctx context.Context, t *testing.T, targets ...Target) []int64 {
	t.Helper()
	devices, detach := h.attachRaw(ctx, t, "size", targets...)
	defer detach()
	sizes := make([]int64, 0, len(devices))
	for _, dev := range devices {
		sizes = append(sizes, int64(dev.SizeBytes))
	}
	return sizes
}

// A logical volume over one namespace is that namespace less
// lvm.PhysicalVolumeOverheadBytes, and no less: the controller pads a volume
// under an LVM stack by exactly that much.
func TestLVMStackKeepsTheOverheadItIsSizedFor(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets[0])
	size := h.namespaceBytes(ctx, t, h.targets[0])[0]

	plan := h.node.LVMRawBlock(h.targets[0].Connection(), h.volume, plans.LogicalVolumeOptions{})
	art := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	got := int64(art.Devices[0].SizeBytes)
	t.Logf("namespace %d bytes, logical volume %d bytes, LVM kept %d", size, got, size-got)
	if want := size - lvm.PhysicalVolumeOverheadBytes; got < want {
		t.Errorf("the logical volume is %d bytes, want at least %d: LVM keeps more than "+
			"lvm.PhysicalVolumeOverheadBytes (%d) of a device", got, want, lvm.PhysicalVolumeOverheadBytes)
	}
}

// A striped logical volume over n namespaces is their sum less n times
// lvm.PhysicalVolumeOverheadBytes.
func TestStripedStackKeepsTheOverheadItIsSizedFor(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	members := h.stripedMembers(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, members...)
	var sum int64
	for _, size := range h.namespaceBytes(ctx, t, members...) {
		sum += size
	}

	plan := h.node.StripedRawBlock(connections(members), h.volume, stripedOptions(len(members)))
	art := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	got := int64(art.Devices[0].SizeBytes)
	t.Logf("members %d bytes together, striped volume %d bytes, LVM kept %d", sum, got, sum-got)
	if want := sum - int64(len(members))*lvm.PhysicalVolumeOverheadBytes; got < want {
		t.Errorf("the striped volume is %d bytes, want at least %d: LVM keeps more than "+
			"lvm.PhysicalVolumeOverheadBytes (%d) of each member", got, want, lvm.PhysicalVolumeOverheadBytes)
	}
}

// What VDO gives a volume of its namespace, recorded rather than asserted: the
// figure the decision about sizing a VDO volume is made from. The volume is
// created the way the driver creates one, with no virtual size, so VDO sizes
// it from the physical space left after its own metadata.
func TestVDOStackRecordsWhatItKeepsOfItsNamespace(t *testing.T) {
	requireLVM(t)
	requireVDO(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets[0])
	size := h.namespaceBytes(ctx, t, h.targets[0])[0]

	plan := h.node.LVMRawBlock(h.targets[0].Connection(), h.volume, plans.LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Deduplication: true, Compression: true},
		PoolName:   vdoPoolName,
	})
	art := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	got := int64(art.Devices[0].SizeBytes)
	t.Logf("VDO: namespace %d bytes (%.2f GiB), volume %d bytes (%.2f GiB), kept %d bytes (%.2f GiB, %.1f%%)",
		size, float64(size)/(1<<30), got, float64(got)/(1<<30), size-got, float64(size-got)/(1<<30),
		100*float64(size-got)/float64(size))
	out, _ := h.node.manager.Run(ctx, "lvs", "--units", "b", "-o", "lv_name,lv_size,data_percent,vdo_operating_mode",
		h.volume.VolumeGroup())
	t.Logf("lvs:\n%s", out)
}
