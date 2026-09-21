//go:build linux

// Naming a staged volume from the host alone.
//
// This is what a teardown falls back on for a volume the previous node service
// staged and never finished recording: no stack record, no stashed volume
// context, and a mount that has to be identified before anything may be
// detached. The unit tests decide when that fallback is consulted; only a real
// kernel can say whether the reading it makes is right.
//
// The mount is made from the by-id symlink on purpose. That is what the
// previous initiator handed back and therefore what a legacy volume is mounted
// from, and it is the difference the case exists to catch: sysfs knows the
// namespace as /dev/nvme0n1, so an identity resolved by comparing device paths
// finds nothing for exactly the volumes this path is for.

package onnode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/volstack"
)

// TestALegacyStackIsReleasedByAReconstructedPlan is the upgrade case that
// happens on every cluster: a volume the previous node service staged has its
// stashed volume context and no stack record, so the teardown rebuilds the plan
// that version could have built — fabric then filesystem — and releases a stack
// nothing in the stack model ever wrote.
//
// It is staged here the way that version staged one, which is the part a fake
// cannot supply: mkfs and mount run against the by-id link the old initiator
// handed back, and no record is written for any of it.
func TestALegacyStackIsReleasedByAReconstructedPlan(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	target := h.targets[0]
	h.blank(ctx, target)

	// Staged the old way: connect, and then the filesystem by hand. The connect
	// goes through a raw-block plan because that is all the fabric half of the
	// old service did, and its record is removed below so that what the teardown
	// meets is a volume with nothing recorded about it.
	attach := h.node.RawBlock(target.Connection())
	artifact := h.up(ctx, attach)
	// Idempotent, and only reached when the release below did not happen: a
	// failing case must not leave the namespace attached for the next one.
	t.Cleanup(func() { _ = h.runner().Down(context.WithoutCancel(ctx), h.handle(), attach) })

	device, ok := artifact.Device()
	if !ok {
		t.Fatal("the connect exposed no device")
	}

	link := byIDLink(t, device.Path)
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("make the staging directory: %v", err)
	}
	fs := shellFilesystem{}
	if err := fs.Format(ctx, link, "ext4", []string{"-F"}); err != nil {
		t.Fatalf("format %s: %v", link, err)
	}
	if err := fs.Mount(ctx, link, staging, "ext4", nil); err != nil {
		t.Fatalf("mount %s at %s: %v", link, staging, err)
	}

	// No record, which is the whole premise: the old service wrote none, and the
	// connect above must not leave one behind either. Removed through the store
	// rather than by deleting a path this test worked out, so that a change to
	// how a record is named cannot turn this into a case that quietly keeps one.
	if err := volstack.NewStore(h.records).Remove(h.handle()); err != nil {
		t.Fatalf("remove the record the connect wrote: %v", err)
	}

	// What the teardown reconstructs from the stashed context: the same
	// connection, and a filesystem layer pointed at the staging path.
	//
	// Built here rather than derived from a stash, because this suite is a
	// module of its own and the derivation lives in the node service's internal
	// packages. What it can drive is the plan those packages hand to the
	// runner, so the derivation itself is pinned by
	// TestALegacyStashIsDerivedIntoATeardownPlan beside it, against a context
	// recorded from a volume that service staged.
	volume := h.volume
	volume.StagingPath = staging
	release := h.node.Plain(target.Connection(), volume)
	if err := h.runner().Down(ctx, h.handle(), release); err != nil {
		t.Fatalf("release a stack this driver did not build: %v", err)
	}

	mounted, err := fs.IsMountPoint(ctx, staging)
	if err != nil {
		t.Fatalf("check whether %s is still mounted: %v", staging, err)
	}
	if mounted {
		t.Error("the staging path is still mounted after the release")
	}

	// Whether the device is gone is not asserted, because the release does not
	// promise it: DetachDevice leaves a subsystem that holds more than one
	// namespace connected, and nvmet reports a namespace budget above one for
	// every subsystem it serves, so the device legitimately survives here. What
	// the teardown owes is the mount above, and the record below.
	if _, err := volstack.NewStore(h.records).Load(h.handle()); !errors.Is(err, volstack.ErrNoRecord) {
		t.Errorf("the release left a record behind: %v", err)
	}

	// Kubelet reissues an unstage it did not get an answer to, and the second
	// one meets a stack that is already down.
	if err := h.runner().Down(ctx, h.handle(), release); err != nil {
		t.Errorf("a second release of the same stack failed: %v", err)
	}
}

// Regression: 2026-09-21-legacy-identity-read-by-device-path. The identity was
// resolved by comparing the mount's device path against the one sysfs records,
// which are two different names for one device: the volumes this path exists
// for are mounted from a by-id link, so the lookup found nothing for every one
// of them.
//
// TestTheHostNamesAVolumeMountedByItsByIDPath brings a namespace up, mounts it
// the way the previous node service did, and asks the host what is staged
// there.
func TestTheHostNamesAVolumeMountedByItsByIDPath(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	target := h.targets[0]
	h.blank(ctx, target)

	// Connect and nothing more: the mount below is this case's, because the
	// path it is made from is what the case is about.
	plan := h.node.RawBlock(target.Connection())
	artifact := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	device, ok := artifact.Device()
	if !ok {
		t.Fatal("the bring-up exposed no device to mount")
	}

	link := byIDLink(t, device.Path)
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("make the staging directory: %v", err)
	}
	fs := shellFilesystem{}
	if err := fs.Format(ctx, link, "ext4", []string{"-F"}); err != nil {
		t.Fatalf("format %s: %v", link, err)
	}
	if err := fs.Mount(ctx, link, staging, "ext4", nil); err != nil {
		t.Fatalf("mount %s at %s: %v", link, staging, err)
	}
	t.Cleanup(func() { _ = fs.Unmount(context.WithoutCancel(ctx), staging) })

	number, err := nvme.DeviceNumberAt(staging)
	if err != nil {
		t.Fatalf("read the device number staged at %s: %v", staging, err)
	}
	named, err := nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{}).
		ByDeviceNumber(ctx, number)
	if err != nil {
		t.Fatalf("name the namespace behind %s (device %s): %v", staging, number, err)
	}

	if named.Subsystem.NQN != target.NQN {
		t.Errorf("the host names subsystem %q, want %q", named.Subsystem.NQN, target.NQN)
	}
	if uint32(named.Namespace.ID) != target.NSID {
		t.Errorf("the host names namespace %d, want %d", named.Namespace.ID, target.NSID)
	}
}

// TestTheHostNamesARawBlockVolumeFromItsDeviceFile is the other shape a staged
// volume takes. A raw block volume is published as a device file rather than
// mounted, so the number to read is the one the file stands for rather than
// the one of the filesystem the file happens to live on.
func TestTheHostNamesARawBlockVolumeFromItsDeviceFile(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	target := h.targets[0]
	plan := h.node.RawBlock(target.Connection())
	artifact := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	device, ok := artifact.Device()
	if !ok {
		t.Fatal("the bring-up exposed no device")
	}

	number, err := nvme.DeviceNumberAt(device.Path)
	if err != nil {
		t.Fatalf("read the device number of %s: %v", device.Path, err)
	}
	named, err := nvme.NewSysfsDeviceResolver(nvme.SysfsConfig{}).
		ByDeviceNumber(ctx, number)
	if err != nil {
		t.Fatalf("name the namespace %s is (device %s): %v", device.Path, number, err)
	}
	if named.Subsystem.NQN != target.NQN {
		t.Errorf("the host names subsystem %q, want %q", named.Subsystem.NQN, target.NQN)
	}
}

// byIDLink is the /dev/disk/by-id link pointing at device, which is the path
// the previous node service mounted a volume from.
func byIDLink(t *testing.T, devicePath string) string {
	t.Helper()

	const byID = "/dev/disk/by-id"
	entries, err := os.ReadDir(byID)
	if err != nil {
		t.Skipf("no %s on this node, and the case is about the link it holds: %v", byID, err)
	}
	for _, entry := range entries {
		link := filepath.Join(byID, entry.Name())
		resolved, err := filepath.EvalSymlinks(link)
		if err != nil || resolved != devicePath {
			continue
		}
		if strings.Contains(entry.Name(), "part") {
			// A partition of the namespace rather than the namespace.
			continue
		}
		return link
	}
	t.Skipf("udev linked no by-id name to %s, and the case is about that link", devicePath)
	return ""
}
