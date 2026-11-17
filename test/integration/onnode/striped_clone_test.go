//go:build linux

// A striped volume restored from a snapshot or cloned from another, on a real
// kernel.
//
// A clone of a striped volume is one clone per member, and every member arrives
// carrying its source's physical-volume label, its source's volume-group name,
// and its source's metadata, which lists every other member by its old identity.
// The volume group spans the members, so its identity is the set's and not any
// one device's: LVM re-stamps a cloned group with vgimportclone over all of its
// physical volumes at once, and a group handed over one device at a time is a
// group with the rest of its devices missing.
//
// The unit tests in atlas cannot tell those two apart. A fake LVM answers a
// per-device import as readily as a whole-set one, so whether the layers resolve
// a cloned set the way LVM accepts is something only a node can show.

package onnode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack/plans"
)

// stripedMembers is the two namespaces a striped case builds its source on.
// Two because that is the fewest a stripe has, and the most the suite publishes
// to a case that does not also need a set to copy into.
func (h *harness) stripedMembers(t *testing.T) []Target {
	t.Helper()
	if len(h.targets) < 2 {
		t.Skip("striping needs a second namespace, and SB_TARGET2_NQN is unset")
	}
	return h.targets[:2]
}

// stripedOptions stripes across n members, said out loud for the reason
// TestStripedStackAssemblesItsMembers gives: a definition left at its zero value
// builds a linear volume that passes every other assertion.
func stripedOptions(n int) plans.LogicalVolumeOptions {
	return plans.LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Stripes: n, StripeChunkBytes: 64 << 10},
	}
}

// cloneVolume is a second volume identity for this case: what the controller
// gives a restore, a new first member and so a new group and volume name, over
// bytes that still carry the source's.
func (h *harness) cloneVolume(t *testing.T) plans.Volume {
	t.Helper()
	return plans.Volume{
		UUID:        h.volume.UUID + "-c",
		StagingPath: filepath.Join(t.TempDir(), "clone-staging"),
		FsType:      h.volume.FsType,
	}
}

// stripedSource brings a striped volume up on members, writes marker into it,
// and releases it again, which is the source a snapshot is taken of.
func (h *harness) stripedSource(ctx context.Context, t *testing.T, members []Target, marker string, want []byte) {
	t.Helper()
	h.blank(ctx, members...)
	plan := h.node.Striped(connections(members), h.volume, stripedOptions(len(members)))
	art := h.up(ctx, plan)
	if err := os.WriteFile(filepath.Join(art.Path, marker), want, 0o600); err != nil {
		t.Fatalf("write into the striped source: %v", err)
	}
	h.down(ctx, plan)
}

// attachRaw attaches each target as a raw block device, for a case that has to
// read or rewrite a member's bytes between bring-ups, and returns the devices
// in target order and the detach.
func (h *harness) attachRaw(ctx context.Context, t *testing.T, tag string, targets ...Target) ([]blockdev.Device, func()) {
	t.Helper()
	devices := make([]blockdev.Device, 0, len(targets))
	var detach []func()
	release := func() {
		for i := len(detach) - 1; i >= 0; i-- {
			detach[i]()
		}
	}
	for i, target := range targets {
		plan := h.node.RawBlock(target.Connection())
		handle := h.volume.UUID + "-" + tag + "-" + strconv.Itoa(i)
		art, err := h.runner().Up(ctx, handle, plan)
		if err != nil {
			release()
			t.Fatalf("attach %s raw: %v", target.NQN, err)
		}
		detach = append(detach, func() {
			if err := h.runner().Down(context.WithoutCancel(ctx), handle, plan); err != nil {
				t.Errorf("detach %s: %v", target.NQN, err)
			}
		})
		if len(art.Devices) != 1 {
			release()
			t.Fatalf("attaching %s exposed %d devices, want one", target.NQN, len(art.Devices))
		}
		devices = append(devices, art.Devices[0])
	}
	return devices, release
}

// copyDevice writes every byte of src over dst, which is what the backend's
// snapshot-and-clone does to a member, done here on the node.
func copyDevice(t *testing.T, src, dst blockdev.Device) {
	t.Helper()
	if src.SizeBytes != dst.SizeBytes {
		t.Fatalf("cannot clone %s (%d bytes) onto %s (%d bytes)", src.Path, src.SizeBytes, dst.Path, dst.SizeBytes)
	}
	in, err := os.Open(src.Path)
	if err != nil {
		t.Fatalf("open %s to copy it: %v", src.Path, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst.Path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s to copy onto it: %v", dst.Path, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy %s onto %s: %v", src.Path, dst.Path, err)
	}
	if err := out.Sync(); err != nil {
		t.Fatalf("flush the copy onto %s: %v", dst.Path, err)
	}
}

// readHead is the first n bytes of a device, read past the page cache's copy of
// what an earlier attachment wrote.
func readHead(t *testing.T, dev blockdev.Device, n int64) []byte {
	t.Helper()
	f, err := os.Open(dev.Path)
	if err != nil {
		t.Fatalf("open %s to read its head: %v", dev.Path, err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, n)
	if _, err := io.ReadFull(f, head); err != nil {
		t.Fatalf("read the first %d bytes of %s: %v", n, dev.Path, err)
	}
	return head
}

// cloneMembers is the backend's snapshot and clone, done on the node: each
// source member's bytes onto a namespace of its own, which is then left
// disconnected, so that a case attaches the clone when it means to.
//
// Disconnected by name, because a detach does not: nvmet subsystems can hold
// more than one namespace, and the fabric layer never disconnects a subsystem
// that could be shared (nvmeof.DetachDevice). Reaping one is the explicit act
// this is.
func (h *harness) cloneMembers(ctx context.Context, t *testing.T, source, copies []Target) {
	t.Helper()
	var copied []string
	func() {
		devices, detach := h.attachRaw(ctx, t, "copy", append(slices.Clone(source), copies...)...)
		defer detach()
		for i := range source {
			copyDevice(t, devices[i], devices[len(source)+i])
			copied = append(copied, devices[len(source)+i].Path)
		}
	}()
	h.disconnect(ctx, t, copies...)
	awaitGone(t, copied)
}

// disconnect reaps the targets' subsystems, which a detach never does here.
func (h *harness) disconnect(ctx context.Context, t *testing.T, targets ...Target) {
	t.Helper()
	connector := nvmeof.NewCLIConnector(nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{}))
	for _, target := range targets {
		if err := connector.Disconnect(ctx, target.NQN); err != nil {
			t.Fatalf("disconnect %s: %v", target.NQN, err)
		}
	}
}

// awaitGone waits for detached devices to leave /dev, and fails the case with
// what is still there when they do not: a namespace the harness believes it
// detached and LVM still scans is a fixture fault, not a finding about the stack.
func awaitGone(t *testing.T, paths []string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var present []string
		for _, path := range paths {
			if _, err := os.Stat(path); err == nil {
				present = append(present, path)
			}
		}
		if len(present) == 0 {
			return
		}
		if time.Now().After(deadline) {
			all, _ := filepath.Glob("/dev/nvme*n*")
			t.Fatalf("%v are still present 30s after their detach; /dev holds %v", present, all)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// lastLine is a report's value line, after whatever notices LVM printed first
// about the case's lvm.conf.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// run runs an LVM command the case composed and fails the case if it fails.
func (h *harness) run(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	out, err := h.node.manager.Run(ctx, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

// groups is every volume group LVM sees on this case's namespaces, by name.
func (h *harness) groups(ctx context.Context, t *testing.T) []string {
	t.Helper()
	var names []string
	for line := range strings.SplitSeq(h.run(ctx, t, "vgs", "--noheadings", "-o", "vg_name"), "\n") {
		if name := strings.TrimSpace(line); name != "" && !strings.Contains(name, " ") {
			names = append(names, name)
		}
	}
	return names
}

// expectStriped asserts what a staged clone has to be, beyond mounting: its own
// group, under its own name, striped across every member, carrying the owner
// tag the source's group carried into it.
func (h *harness) expectStriped(ctx context.Context, t *testing.T, volume plans.Volume, members int) {
	t.Helper()
	lv := volume.VolumeGroup() + "/" + volume.LogicalVolume()
	stripes := lastLine(h.run(ctx, t, "lvs", "--noheadings", "-o", "stripes", lv))
	if want := strconv.Itoa(members); stripes != want {
		t.Errorf("%s has %s stripes, want %s", lv, stripes, want)
	}
	tags := tagsOfLastLine(h.run(ctx, t, "vgs", "--noheadings", "-o", "vg_tags", volume.VolumeGroup()))
	if !slices.Contains(tags, lvm.OwnerTag) {
		t.Errorf("the group %s carries %v and not %s", volume.VolumeGroup(), tags, lvm.OwnerTag)
	}
}

// A restore of a striped snapshot is the members' bytes under a new first
// member's identity. Staging it has to re-stamp the volume group as one set,
// rename the logical volume inside, and mount the source's data.
//
// Regression: 2026-11-17-striped-clone-vgimportclone (PR #922 review). The
// physical-volume layer resolves a clone one device at a time, a vgimportclone
// scoped to that device alone. A group spanning several members cannot be
// imported from one of them, so every restore and every clone of a striped
// volume failed to stage, and the controller tests, which stop at the control
// plane, reported clone and restore as working.
func TestStripedStackRestoresAClone(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	members := h.stripedMembers(t)
	want := []byte("written to the source before the snapshot")
	h.stripedSource(ctx, t, members, "source", want)

	// The same bytes, staged as the restore: what the members of a clone are,
	// minus the copy, which a second set of namespaces would only repeat.
	clone := h.cloneVolume(t)
	plan := h.node.Striped(connections(members), clone, stripedOptions(len(members)))
	art, err := h.runner().Up(ctx, clone.UUID, plan)
	if err != nil {
		t.Fatalf("a restore of a striped volume could not be staged: %v", err)
	}
	t.Cleanup(func() {
		if err := h.runner().Down(context.WithoutCancel(ctx), clone.UUID, plan); err != nil {
			t.Errorf("release the restore: %v", err)
		}
	})

	got, err := os.ReadFile(filepath.Join(art.Path, "source")) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatalf("the restore does not hold the source's data: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the restore reads %q, want %q", got, want)
	}
	h.expectStriped(ctx, t, clone, len(members))
	if groups := h.groups(ctx, t); slices.Contains(groups, h.volume.VolumeGroup()) {
		t.Errorf("the source's group %s is still on the members after the restore took them: %v",
			h.volume.VolumeGroup(), groups)
	}
}

// A clone whose re-identification stopped partway, one member under the
// clone's group and the other still under the source's, is two halves of two
// different groups. Importing the stale half on its own would make it a group
// of its own, and every later bring-up would assemble a stripe with a hole in
// it. It is refused, and neither member's metadata is changed by the refusal.
//
// Not red before the fix: the unchanged layer refuses this shape too, because a
// per-device import of the stale half fails for the same reason it fails on a
// whole clone. What this case pins is that a fix resolving the set as one does
// not resolve a mixed set by importing whatever is left.
func TestStripedStackRefusesAHalfImportedClone(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	members := h.stripedMembers(t)
	h.stripedSource(ctx, t, members, "source", []byte("the source's data"))
	clone := h.cloneVolume(t)

	// The shape, made by hand: the whole set imported under the clone's name,
	// then the second member's metadata area put back the way the source left
	// it, which is what an import that wrote one device and died leaves.
	var before [][]byte
	func() {
		devices, detach := h.attachRaw(ctx, t, "reshape", members...)
		defer detach()
		peStart, err := strconv.ParseInt(lastLine(h.run(ctx, t,
			"pvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "pe_start", devices[1].Path)), 10, 64)
		if err != nil || peStart <= 0 {
			t.Fatalf("read where %s's data begins: %d, %v", devices[1].Path, peStart, err)
		}
		saved := readHead(t, devices[1], peStart)
		h.run(ctx, t, "vgimportclone", "--basevgname", clone.VolumeGroup(), devices[0].Path, devices[1].Path)
		writeAt(t, devices[1], 0, saved)
		_, _ = h.node.manager.Run(ctx, "pvscan", "--cache")
		before = [][]byte{readHead(t, devices[0], peStart), readHead(t, devices[1], peStart)}
	}()

	plan := h.node.Striped(connections(members), clone, stripedOptions(len(members)))
	_, err := h.runner().Up(ctx, clone.UUID, plan)
	// Released either way: a refused bring-up still attached the members first.
	_ = h.runner().Down(context.WithoutCancel(ctx), clone.UUID, plan)
	if err == nil {
		t.Fatal("a clone with one member under each group was brought up")
	}
	t.Logf("refused, as it must: %v", err)

	devices, detach := h.attachRaw(ctx, t, "readback", members...)
	defer detach()
	for i, dev := range devices {
		if after := readHead(t, dev, int64(len(before[i]))); !bytes.Equal(after, before[i]) {
			t.Errorf("the refusal rewrote member %d's metadata (%s)", i, dev.Path)
		}
	}
}

// A clone staged on the host that is staging its source. Both sets carry the
// same physical-volume identities until the clone's is re-stamped, so LVM sees
// every source member twice, and only scoping a command to the clone's devices
// says which copy it means. The clone comes up as its own group, the source
// stays up and untouched, and a write to one is not seen by the other.
//
// Regression: 2026-11-17-striped-clone-vgimportclone (PR #922 review), on the
// host where it costs most: a re-identification that reached the source's
// devices instead of the clone's would rename a live volume group.
func TestStripedStackStagesACloneBesideItsSource(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	if len(h.targets) < 4 {
		t.Skip("a clone beside its source needs four namespaces, and SB_TARGET4_NQN is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	source, copies := h.targets[:2], h.targets[2:4]
	want := []byte("written to the source before the snapshot")
	h.stripedSource(ctx, t, source, "source", want)

	// The source is staged with the copies detached, so that what this case
	// tests is the clone attaching beside a live source, and not the source
	// re-staging beside a clone nobody has re-identified yet
	// (TestStripedStackRestagesItsSourceBesideAnUnresolvedClone).
	h.cloneMembers(ctx, t, source, copies)

	sourcePlan := h.node.Striped(connections(source), h.volume, stripedOptions(len(source)))
	sourceArt := h.up(ctx, sourcePlan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), sourcePlan) })

	clone := h.cloneVolume(t)
	clonePlan := h.node.Striped(connections(copies), clone, stripedOptions(len(copies)))
	cloneArt, err := h.runner().Up(ctx, clone.UUID, clonePlan)
	if err != nil {
		t.Fatalf("a clone of a striped volume could not be staged beside its source: %v", err)
	}
	t.Cleanup(func() {
		if err := h.runner().Down(context.WithoutCancel(ctx), clone.UUID, clonePlan); err != nil {
			t.Errorf("release the clone: %v", err)
		}
	})

	got, err := os.ReadFile(filepath.Join(cloneArt.Path, "source")) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatalf("the clone does not hold the source's data: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the clone reads %q, want %q", got, want)
	}
	h.expectStriped(ctx, t, clone, len(copies))
	h.expectStriped(ctx, t, h.volume, len(source))

	// Two volumes, not one seen twice.
	if err := os.WriteFile(filepath.Join(cloneArt.Path, "clone-only"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write into the clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceArt.Path, "clone-only")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file written to the clone is visible in the source: %v", err)
	}
}

// A source re-staged on a host where a clone of it is attached and not yet
// re-identified: a clone stage between its attach and its import, or one whose
// import was refused and left its members attached. Until the import, every
// source member's physical-volume identity is on two devices. The source's own
// bring-up names its own devices, so it is unaffected: it comes up, serves its
// data, and goes down again, and the clone's members are left exactly as they
// were.
//
// Regression: 2026-11-17-striped-activation-unscoped (PR #922 review). The
// volume-group layer activates and deactivates its group by name alone, so LVM
// considers every device carrying the group's physical volumes, finds each one
// twice, and refuses ("Cannot activate LVs in VG ... while PVs appear on
// duplicate devices"). A pod on the source could not be restarted on that node
// while its clone was being staged there. Found by accident in the first run of
// TestStripedStackStagesACloneBesideItsSource.
func TestStripedStackRestagesItsSourceBesideAnUnresolvedClone(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	if len(h.targets) < 4 {
		t.Skip("a clone beside its source needs four namespaces, and SB_TARGET4_NQN is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	source, copies := h.targets[:2], h.targets[2:4]
	want := []byte("written to the source before the snapshot")
	h.stripedSource(ctx, t, source, "source", want)
	h.cloneMembers(ctx, t, source, copies)

	// Whatever a failed bring-up left of the source is released last, once the
	// clone is gone again and its duplicates with it, so that a red run does not
	// leave the next case a host with a half-staged stack on it. Cleanups run in
	// reverse, so this is registered first.
	plan := h.node.Striped(connections(source), h.volume, stripedOptions(len(source)))
	t.Cleanup(func() { _ = h.runner().Down(context.WithoutCancel(ctx), h.handle(), plan) })

	// The clone attached and nothing more: its members carry the source's
	// identities, byte for byte.
	clones, detach := h.attachRaw(ctx, t, "unresolved", copies...)
	t.Cleanup(func() {
		detach()
		h.disconnect(context.WithoutCancel(ctx), t, copies...)
	})
	const head = 1 << 20
	before := [][]byte{readHead(t, clones[0], head), readHead(t, clones[1], head)}

	art, err := h.runner().Up(ctx, h.handle(), plan)
	if err != nil {
		t.Fatalf("the source could not be staged beside an attached clone of it: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(art.Path, "source")) //nolint:gosec // a path the test made
	if err != nil {
		t.Errorf("the re-staged source does not hold its data: %v", err)
	} else if !bytes.Equal(got, want) {
		t.Errorf("the re-staged source reads %q, want %q", got, want)
	}
	if err := h.runner().Down(ctx, h.handle(), plan); err != nil {
		t.Fatalf("the source could not be released beside an attached clone of it: %v", err)
	}

	for i, dev := range clones {
		if after := readHead(t, dev, head); !bytes.Equal(after, before[i]) {
			t.Errorf("staging the source rewrote the clone's member %d (%s)", i, dev.Path)
		}
	}
}
