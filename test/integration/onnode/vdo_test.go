//go:build linux

// A VDO volume whose creation was interrupted, and what the next bring-up does
// about what it left behind.
//
// lvcreate --type vdo is several commits, not one: the pool is made as a plain
// logical volume, formatted, converted, and only then does the volume inside it
// appear. A node that dies between the first commit and the last leaves its
// group holding `vdopool` and nothing else. That is what a storage node's crash
// mid-stage left behind on 2026-09-26 (e2e run 36219557238): the layer above
// read the group as an interrupted create, which it was, and ran the same
// lvcreate again, which LVM refuses because the pool's name is taken. Every
// retry after that refused the same way, and the volume was unstageable until
// somebody removed the pool by hand.
//
// The interruption is produced at the same point, by the same call, rather than
// by building the shape by hand. The bring-up runs for real up to the lvcreate
// that makes the pool, and the runner beneath it performs only LVM's own first
// step before reporting the process gone. What the second bring-up then finds is
// what the driver found after the reboot, including whatever the layer writes
// ahead of a create to say that it is in the middle of one, so a recovery gated
// on that marker is exercised without this case knowing the marker's name.
//
// Talos builds no dm-vdo, so on the cluster the suite usually runs on these
// cases skip. The integration workflow's runner job exists to run them on a
// kernel that carries the target.

package onnode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/volstack/plans"
)

// vdoPoolName is the pool `lvcreate --type vdo` creates alongside the logical
// volume, inside every volume's own group. The driver names it the same way,
// in a constant this suite cannot import, and the name is structural rather than
// per volume: uniqueness comes from the group.
const vdoPoolName = "vdopool"

// errInterrupted is what the dying runner reports in place of lvcreate's
// result. A process that was killed reports nothing at all, and the closest an
// in-process stand-in can come is an error the layer did not anticipate.
var errInterrupted = errors.New("the node died inside lvcreate, after the pool's first commit")

// requireVDO skips a case whose stack needs the VDO device-mapper target when
// the kernel cannot provide it or the image cannot format a pool for it.
//
// The module goes by two names: dm-vdo since it entered the mainline kernel in
// 6.9, and kvdo for the out-of-tree build the Red Hat kernels carry. Both are
// tried, because the node plugin's own capability probe tries both, and a case
// that tried one would skip on kernels the plugin serves.
//
// A skip and not a failure, for the reason requireLVM gives: the kernel is the
// node's, and a suite that cannot change it can only say so. The run that has to
// prove these cases is the one on a kernel carrying the target, and the
// integration workflow's runner job fails rather than skips when they do not run.
func requireVDO(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("vdoformat"); err != nil {
		t.Skip("the image carries no vdoformat, which lvcreate --type vdo needs to format a pool")
	}
	var last error
	for _, module := range []string{"dm-vdo", "kvdo"} {
		out, err := exec.Command("modprobe", module).CombinedOutput()
		if err == nil {
			return
		}
		last = fmt.Errorf("modprobe %s: %w: %s", module, err, strings.TrimSpace(string(out)))
	}
	t.Skipf("the kernel provides no VDO target: %v", last)
}

// dyingInsidePoolCreate is the LVM seam for a node that dies inside the one
// lvcreate that makes a VDO pool, after the pool exists and before the volume
// inside it does.
//
// Every other command runs for real, so the physical volume, the group, and
// anything the layer records ahead of the create are the genuine article. Only
// the create itself is cut short, and it is cut at LVM's own first commit: the
// pool as a plain logical volume of the size the create asked for, which is the
// state lvm2 passes through before vdoformat runs.
func dyingInsidePoolCreate() *lvm.Manager {
	real := lvm.NewManager()
	return lvm.NewManagerWithRunner(func(ctx context.Context, args ...string) (string, error) {
		if len(args) == 0 || args[0] != "lvcreate" || !slices.Contains(args, "vdo") {
			return real.Run(ctx, args...)
		}
		target := poolTarget(args)
		group, pool, ok := strings.Cut(target, "/")
		if !ok {
			return "", fmt.Errorf("lvcreate names no <group>/<pool> target in %v", args)
		}
		if out, err := real.Run(ctx, "lvcreate", "-n", pool, "-l", "100%FREE", "--yes", group); err != nil {
			return out, fmt.Errorf("make the pool the way lvm2's first commit does: %w", err)
		}
		return "", errInterrupted
	})
}

// poolTarget is lvcreate's <group>/<pool> operand, the one argument carrying a
// slash: the volume's name, the extent count, and the type are all flag values
// without one.
func poolTarget(args []string) string {
	for _, arg := range args[1:] {
		if strings.Contains(arg, "/") {
			return arg
		}
	}
	return ""
}

// logicalVolumesIn is what LVM lists in a group, read over a fabric attachment
// of this case's own, because the bring-up that made the group has released it
// by the time there is anything to ask.
func (h *harness) logicalVolumesIn(ctx context.Context, target Target, group string) []string {
	h.t.Helper()
	plan := h.node.RawBlock(target.Connection())
	handle := h.volume.UUID + "-inspect"
	if _, err := h.runner().Up(ctx, handle, plan); err != nil {
		h.t.Fatalf("attach %s in order to read its group: %v", target.NQN, err)
	}
	defer func() {
		if err := h.runner().Down(ctx, handle, plan); err != nil {
			h.t.Fatalf("detach %s after reading its group: %v", target.NQN, err)
		}
	}()
	out, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "lv_name", group)
	if err != nil {
		h.t.Fatalf("list the logical volumes in %s: %v", group, err)
	}
	return strings.Fields(out)
}

// Regression: 2026-09-26-vdopool-already-exists-after-interrupted-lvcreate (e2e
// run 36219557238). The interrupted create is the case the layer's Partial state
// was written for, and for a pooled type the retry it performed could not succeed: the pool the first
// attempt made is still there, under the name the second attempt needs. A
// bring-up that converges has to notice the pool and deal with it, and a
// bring-up that does not is a volume no node can stage.
func TestVDOStackConvergesAfterAnInterruptedPoolCreate(t *testing.T) {
	requireLVM(t)
	requireVDO(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets[0])

	// Deduplication alone, which is what the failed e2e volume asked for. Either
	// switch needs the pool, and the pool is what this case is about.
	options := plans.LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Deduplication: true},
		PoolName:   vdoPoolName,
	}
	connection := h.targets[0].Connection()

	// The first bring-up, on a node that dies inside lvcreate. Up releases what
	// it had brought up before returning, which is the closest a process can
	// come to a reboot: nothing mapped, nothing connected, and the record left
	// behind for the next attempt.
	dying := newNodeWithManager(h.node.hostNQN, h.node.hostID, dyingInsidePoolCreate())
	_, err := h.runner().Up(ctx, h.handle(), dying.LVM(connection, h.volume, options))
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("the bring-up was meant to die inside lvcreate, and instead: %v", err)
	}

	// What that left is the shape the crash left: the group, holding the pool
	// and nothing else. Asserted before the second bring-up runs, so that a
	// failure below is about the recovery and not about the setup.
	if got := h.logicalVolumesIn(ctx, h.targets[0], h.volume.VolumeGroup()); !slices.Equal(got, []string{vdoPoolName}) {
		t.Fatalf("the interrupted create left %v in %s, want only the pool %q",
			got, h.volume.VolumeGroup(), vdoPoolName)
	}

	// The second bring-up is kubelet's retry of NodeStageVolume, on the node as
	// it comes back: the real LVM, and a plan identical to the first.
	plan := h.node.LVM(connection, h.volume, options)
	art, err := h.runner().Up(ctx, h.handle(), plan)
	if err != nil {
		t.Fatalf("the second bring-up did not converge over the interrupted create: %v", err)
	}
	t.Cleanup(func() { h.down(context.WithoutCancel(ctx), plan) })

	if art.Path != h.volume.StagingPath {
		t.Fatalf("mounted at %q, want %q", art.Path, h.volume.StagingPath)
	}
	marker := filepath.Join(art.Path, "written-after-the-recovery")
	if err := os.WriteFile(marker, []byte("usable"), 0o600); err != nil {
		t.Fatalf("write into the recovered volume: %v", err)
	}

	// LVM's own account, because a mount alone is satisfied by a plain volume:
	// the pool is a real VDO pool now, and the volume the plan named is in it.
	out, err := h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "segtype",
		h.volume.VolumeGroup()+"/"+vdoPoolName)
	if err != nil {
		t.Fatalf("read the pool's segment type: %v", err)
	}
	if got := strings.TrimSpace(out); got != "vdo-pool" {
		t.Errorf("%s/%s is a %q segment, want vdo-pool: the recovery did not rebuild the pool as VDO",
			h.volume.VolumeGroup(), vdoPoolName, got)
	}
	out, err = h.node.manager.Run(ctx, "lvs", "--noheadings", "-o", "lv_name", h.volume.VolumeGroup())
	if err != nil {
		t.Fatalf("list the logical volumes in %s: %v", h.volume.VolumeGroup(), err)
	}
	if got := strings.Fields(out); !slices.Contains(got, h.volume.LogicalVolume()) {
		t.Errorf("%s holds %v, and the volume %q is not among them",
			h.volume.VolumeGroup(), got, h.volume.LogicalVolume())
	}
}

// Regression: 2026-09-26-vdopool-already-exists-after-interrupted-lvcreate, the
// negative half. The recovery above removes a pool, and the only thing that makes
// that safe is the reading it is gated on: the pool alone, under the marker. This is the
// other reading, and the one that costs data if it is ever converged over: the
// same group, the same marker, and a volume beside the pool that is not ours,
// which is what an interrupted clone resolution leaves. The bring-up has to
// refuse, and refusing has to leave the volume and its bytes where they were.
//
// Not red before the fix: the unchanged layer refused this shape too, by
// accident, because the pool's name blocked its lvcreate. What this case pins is
// that the recovery did not widen that into a removal.
func TestVDOStackRefusesAPoolBesideSomebodysVolume(t *testing.T) {
	requireLVM(t)
	requireVDO(t)
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), stackTimeout)
	defer cancel()

	h.blank(ctx, h.targets[0])

	options := plans.LogicalVolumeOptions{
		Definition: lvm.LogicalVolumeDefinition{Deduplication: true},
		PoolName:   vdoPoolName,
	}
	connection := h.targets[0].Connection()

	// A complete volume first, so that there is data to lose.
	plan := h.node.LVM(connection, h.volume, options)
	art := h.up(ctx, plan)
	want := []byte("somebody's data")
	if err := os.WriteFile(filepath.Join(art.Path, "theirs"), want, 0o600); err != nil {
		t.Fatalf("write into the staged filesystem: %v", err)
	}
	h.down(ctx, plan)

	// Then the shape: the volume renamed out from under its own name, as a
	// clone import that died before renaming it back would leave it, and the
	// marker on the group, as the most permissive reading the gate could meet.
	h.overGroup(ctx, h.targets[0], func(group string) {
		run := func(args ...string) {
			if out, err := h.node.manager.Run(ctx, args...); err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
		}
		run("vgchange", "-an", group)
		run("lvrename", group, h.volume.LogicalVolume(), "somebody-elses")
		run("vgchange", "--addtag", "simplyblock.creating", group)
	})

	_, err := h.runner().Up(ctx, h.handle(), plan)
	if err == nil {
		t.Fatal("a group holding somebody's volume beside the pool was converged over")
	}
	t.Logf("refused, as it must: %v", err)

	// Untouched: the volume is still there under its name, and so are its bytes.
	got := h.logicalVolumesIn(ctx, h.targets[0], h.volume.VolumeGroup())
	slices.Sort(got)
	if want := []string{"somebody-elses", vdoPoolName}; !slices.Equal(got, want) {
		t.Fatalf("the refusal changed the group: holds %v, want %v", got, want)
	}
	h.overGroup(ctx, h.targets[0], func(group string) {
		run := func(args ...string) string {
			out, err := h.node.manager.Run(ctx, args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
			return out
		}
		run("vgchange", "-ay", group)
		defer run("vgchange", "-an", group)
		mount := t.TempDir()
		if err := (shellFilesystem{}).Mount(ctx, "/dev/"+group+"/somebody-elses", mount, h.volume.FsType, nil); err != nil {
			t.Fatalf("mount the renamed volume to read it back: %v", err)
		}
		defer func() { _ = (shellFilesystem{}).Unmount(ctx, mount) }()
		got, err := os.ReadFile(filepath.Join(mount, "theirs")) //nolint:gosec // a path the test made
		if err != nil {
			t.Fatalf("the refusal lost the volume's data: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("the volume reads %q after the refusal, want %q", got, want)
		}
	})
}

// overGroup attaches the namespace, hands the group's name to fn, and detaches
// again, for a case that has to reshape a group by hand between bring-ups.
func (h *harness) overGroup(ctx context.Context, target Target, fn func(group string)) {
	h.t.Helper()
	plan := h.node.RawBlock(target.Connection())
	handle := h.volume.UUID + "-reshape"
	if _, err := h.runner().Up(ctx, handle, plan); err != nil {
		h.t.Fatalf("attach %s in order to reshape its group: %v", target.NQN, err)
	}
	defer func() {
		if err := h.runner().Down(ctx, handle, plan); err != nil {
			h.t.Fatalf("detach %s after reshaping its group: %v", target.NQN, err)
		}
	}()
	fn(h.volume.VolumeGroup())
}
