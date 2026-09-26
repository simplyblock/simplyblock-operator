//go:build linux

// Ownership on a real kernel: what the tag lets a bring-up touch, and what it
// keeps a bring-up away from.
//
// The unit tests prove the layers decide correctly given what a fake told them.
// What only a node can prove is that LVM writes the tag where the layers expect
// to read it back, that vgimportclone carries it into a clone, and that a refusal
// leaves a group exactly as it was, bytes included.

package onnode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack/plans"
)

// groupTags is what LVM lists for the group, read over an attachment of this
// case's own.
func (h *harness) groupTags(ctx context.Context, target Target, group string) []string {
	h.t.Helper()
	var tags []string
	h.overGroup(ctx, target, func(string) {
		out, err := h.node.manager.Run(ctx, "vgs", "--noheadings", "-o", "vg_tags", group)
		if err != nil {
			h.t.Fatalf("read the tags of %s: %v", group, err)
		}
		for tag := range strings.SplitSeq(strings.TrimSpace(out), ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				tags = append(tags, tag)
			}
		}
	})
	return tags
}

// A group the driver makes carries the tag from the moment it exists, and so
// does the volume inside it.
func TestLVMStackTagsWhatItMakes(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets[0])
	plan := h.node.LVM(h.targets[0].Connection(), h.volume, plans.LogicalVolumeOptions{})
	h.up(ctx, plan)
	h.down(ctx, plan)

	if tags := h.groupTags(ctx, h.targets[0], h.volume.VolumeGroup()); !slices.Contains(tags, lvm.OwnerTag) {
		t.Fatalf("the group carries %v and not %s", tags, lvm.OwnerTag)
	}
	if tags := h.groupTags(ctx, h.targets[0], h.volume.VolumeGroup()); !slices.Contains(tags, lvm.InformationalTag("lvol", h.volume.UUID)) {
		t.Fatalf("the group carries %v and not its lvol tag", tags)
	}
	h.overGroup(ctx, h.targets[0], func(group string) {
		out, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "lv_tags", group+"/"+h.volume.LogicalVolume())
		if err != nil {
			t.Fatalf("read the volume's tags: %v", err)
		}
		if !strings.Contains(out, lvm.OwnerTag) {
			t.Fatalf("the volume carries %q and not %s", strings.TrimSpace(out), lvm.OwnerTag)
		}
	})
}

// A volume made before the tag existed is the driver's all the same, and the
// next bring-up has to know it by its shape: complete, under our names. It is
// adopted, tagged, and serves its data.
func TestLVMStackAdoptsAVolumeMadeBeforeTheTag(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets[0])
	plan := h.node.LVM(h.targets[0].Connection(), h.volume, plans.LogicalVolumeOptions{})
	art := h.up(ctx, plan)
	want := []byte("from before the tag")
	if err := os.WriteFile(filepath.Join(art.Path, "old"), want, 0o600); err != nil {
		t.Fatalf("write into the staged filesystem: %v", err)
	}
	h.down(ctx, plan)

	// Strip everything the driver wrote, which is what a volume from an older
	// driver looks like.
	h.overGroup(ctx, h.targets[0], func(group string) {
		for _, tag := range h.groupTagsNow(ctx, group) {
			if strings.HasPrefix(tag, lvm.OwnerTag) {
				if out, err := h.node.manager.Run(ctx, "vgchange", "--deltag", tag, group); err != nil {
					t.Fatalf("strip %s: %v\n%s", tag, err, out)
				}
			}
		}
		if out, err := h.node.manager.Run(ctx, "lvchange", "--deltag", lvm.OwnerTag, group); err != nil {
			t.Fatalf("strip the volume's tag: %v\n%s", err, out)
		}
	})

	again := h.up(ctx, plan)
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })
	got, err := os.ReadFile(filepath.Join(again.Path, "old")) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatalf("the adopted volume lost its data: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the adopted volume reads %q, want %q", got, want)
	}
	h.down(ctx, plan)
	if tags := h.groupTags(ctx, h.targets[0], h.volume.VolumeGroup()); !slices.Contains(tags, lvm.OwnerTag) {
		t.Fatalf("the adopted group carries %v and not %s", tags, lvm.OwnerTag)
	}
	h.up(ctx, plan)
}

// groupTagsNow is groupTags for a caller already holding an attachment.
func (h *harness) groupTagsNow(ctx context.Context, group string) []string {
	h.t.Helper()
	out, err := h.node.manager.Run(ctx, "vgs", "--noheadings", "-o", "vg_tags", group)
	if err != nil {
		h.t.Fatalf("read the tags of %s: %v", group, err)
	}
	var tags []string
	for tag := range strings.SplitSeq(strings.TrimSpace(out), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// A group that is not the driver's is left as it was found, whatever it is
// called. Two shapes: a group under our own name holding somebody's volume,
// which the group layer meets, and a group under another name that the clone
// path would otherwise re-identify. Both are refused, and both read back
// unchanged afterward.
func TestLVMStackRefusesAGroupThatIsNotItsOwn(t *testing.T) {
	requireLVM(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	for name, group := range map[string]string{
		"under our name":     h.volume.VolumeGroup(),
		"under another name": "somebody-elses",
	} {
		t.Run(name, func(t *testing.T) {
			h.blank(ctx, h.targets[0])

			// Made by hand, the way anything that is not the driver makes it: a
			// label, a group, and a volume with a filesystem and a file on it.
			var device string
			h.overGroup(ctx, h.targets[0], func(string) {
				devices := h.attachedDevices(ctx)
				if len(devices) != 1 {
					t.Fatalf("attached %d devices, want one", len(devices))
				}
				device = devices[0]
				for _, args := range [][]string{
					{"pvcreate", device},
					{"vgcreate", group, device},
					{"lvcreate", "-n", "theirs", "-l", "100%FREE", "--yes", group},
				} {
					if out, err := h.node.manager.Run(ctx, args...); err != nil {
						t.Fatalf("%v: %v\n%s", args, err, out)
					}
				}
				mount := t.TempDir()
				path := "/dev/" + group + "/theirs"
				if out, err := h.node.manager.Run(ctx, "mkfs.ext4", "-q", path); err != nil {
					t.Fatalf("mkfs: %v\n%s", err, out)
				}
				if err := (shellFilesystem{}).Mount(ctx, path, mount, "ext4", nil); err != nil {
					t.Fatalf("mount: %v", err)
				}
				if err := os.WriteFile(filepath.Join(mount, "theirs"), []byte("not ours"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				if err := (shellFilesystem{}).Unmount(ctx, mount); err != nil {
					t.Fatalf("unmount: %v", err)
				}
				if out, err := h.node.manager.Run(ctx, "vgchange", "-an", group); err != nil {
					t.Fatalf("deactivate: %v\n%s", err, out)
				}
			})

			plan := h.node.LVM(h.targets[0].Connection(), h.volume, plans.LogicalVolumeOptions{})
			_, err := h.runner().Up(ctx, h.handle(), plan)
			if err == nil {
				t.Fatal("a group that is not the driver's was brought up")
			}
			t.Logf("refused, as it must: %v", err)

			// Unchanged: same group, same volume, same bytes, and still no tag.
			h.overGroup(ctx, h.targets[0], func(string) {
				out, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "vg_name,lv_name,vg_tags", group)
				if err != nil {
					t.Fatalf("read the group back: %v", err)
				}
				fields := strings.Fields(out)
				if len(fields) < 2 || fields[0] != group || fields[1] != "theirs" || strings.Contains(out, lvm.OwnerTag) {
					t.Fatalf("the refusal changed the group: %q", strings.TrimSpace(out))
				}
				if out, err := h.node.manager.Run(ctx, "vgchange", "-ay", group); err != nil {
					t.Fatalf("activate to read the bytes back: %v\n%s", err, out)
				}
				defer func() { _, _ = h.node.manager.Run(ctx, "vgchange", "-an", group) }()
				mount := t.TempDir()
				if err := (shellFilesystem{}).Mount(ctx, "/dev/"+group+"/theirs", mount, "ext4", nil); err != nil {
					t.Fatalf("mount: %v", err)
				}
				defer func() { _ = (shellFilesystem{}).Unmount(ctx, mount) }()
				got, err := os.ReadFile(filepath.Join(mount, "theirs")) //nolint:gosec // a path the test made
				if err != nil || string(got) != "not ours" {
					t.Fatalf("the refusal lost the data: %q, %v", got, err)
				}
			})
		})
	}
}

// attachedDevices is the namespace devices the current raw attachment exposes.
func (h *harness) attachedDevices(ctx context.Context) []string {
	h.t.Helper()
	art, err := h.runner().Observe(ctx, h.node.RawBlock(h.targets[0].Connection()))
	if err != nil {
		h.t.Fatalf("observe the attachment: %v", err)
	}
	paths := make([]string, 0, len(art.Devices))
	for _, d := range art.Devices {
		paths = append(paths, d.Path)
	}
	return paths
}
