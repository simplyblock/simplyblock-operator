// What the ownership check refuses, what it lets through, and what adoption and
// the informational tags issue.

package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Every mutation on a group, and the command it must not issue when the group
// is not the driver's.
func TestMutationsRefuseAGroupWithoutTheOwnerTag(t *testing.T) {
	vg := VolumeGroup{Name: "vg1"}
	lv := LogicalVolume{VolumeGroup: vg, Name: "lv1"}
	pv := PhysicalVolume{DevicePath: "/dev/nvme0n1"}
	for name, call := range map[string]func(*Manager) error{
		"ActivateVolumeGroup":   func(m *Manager) error { return m.ActivateVolumeGroup(context.Background(), vg) },
		"DeactivateVolumeGroup": func(m *Manager) error { return m.DeactivateVolumeGroup(context.Background(), vg) },
		"RemoveVolumeGroup":     func(m *Manager) error { return m.RemoveVolumeGroup(context.Background(), vg) },
		"ExtendVolumeGroup":     func(m *Manager) error { return m.ExtendVolumeGroup(context.Background(), vg, pv) },
		"CreateLogicalVolume": func(m *Manager) error {
			_, err := m.CreateLogicalVolume(context.Background(), vg, "", "lv1", LogicalVolumeDefinition{})
			return err
		},
		"RemoveLogicalVolume":       func(m *Manager) error { return m.RemoveLogicalVolume(context.Background(), lv) },
		"ExpandLogicalVolume":       func(m *Manager) error { return m.ExpandLogicalVolume(context.Background(), lv) },
		"ExtendLogicalVolumeToSize": func(m *Manager) error { return m.ExtendLogicalVolumeToSize(context.Background(), lv, 4096) },
		"RenameLogicalVolume":       func(m *Manager) error { return m.RenameLogicalVolume(context.Background(), vg, "a", "b") },
		"AddVolumeGroupTag":         func(m *Manager) error { return m.AddVolumeGroupTag(context.Background(), vg, CreatingMarker) },
		"RemoveVolumeGroupTag":      func(m *Manager) error { return m.RemoveVolumeGroupTag(context.Background(), vg, CreatingMarker) },
		"ReconcileInformationalTags": func(m *Manager) error {
			return m.ReconcileInformationalTags(context.Background(), vg, nil)
		},
		"RemovePhysicalVolume":    func(m *Manager) error { return m.RemovePhysicalVolume(context.Background(), pv) },
		"ExpandPhysicalVolume":    func(m *Manager) error { return m.ExpandPhysicalVolume(context.Background(), pv) },
		"ImportClonedVolumeGroup": func(m *Manager) error { return m.ImportClonedVolumeGroup(context.Background(), vg, pv) },
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}, unowned: true}
			// The device-addressed checks read the group off the device: a group
			// named, and no tag on it.
			fake.out[joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name,vg_tags", "/dev/nvme0n1"})] = "  vg1\n"
			err := call(NewManagerWithRunner(fake.run))
			if !errors.Is(err, ErrNotOwned) {
				t.Fatalf("error = %v, want ErrNotOwned", err)
			}
			if got := fake.mutating(); len(got) != 0 {
				t.Fatalf("the refusal issued %v", got)
			}
		})
	}
}

// A device carrying no group, or no label at all, has nothing to protect: the
// removals behind it are convergent and the layer above decided what may be
// labeled. So the device-addressed mutations pass on either reading.
func TestDeviceMutationsPassAnOrphanOrBlankDevice(t *testing.T) {
	pv := PhysicalVolume{DevicePath: "/dev/nvme0n1"}
	key := joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name,vg_tags", "/dev/nvme0n1"})
	for name, tt := range map[string]struct {
		out string
		err error
	}{
		"orphan label": {out: "  \n"},
		"no label":     {err: errors.New("Failed to find physical volume \"/dev/nvme0n1\".")},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRunner{out: map[string]string{key: tt.out}, err: map[string]error{key: tt.err}, unowned: true}
			if err := NewManagerWithRunner(fake.run).RemovePhysicalVolume(context.Background(), pv); err != nil {
				t.Fatalf("RemovePhysicalVolume: %v", err)
			}
			if got := fake.mutating(); len(got) != 1 || got[0][0] != "pvremove" {
				t.Fatalf("issued %v, want the pvremove", got)
			}
		})
	}
}

// A removal asked for a group that is not there has done what it was asked, the
// way every removal in this package already read that answer. The check does
// not turn a convergent removal into an error.
func TestRemovalsStayConvergentWhenTheGroupIsGone(t *testing.T) {
	vg := VolumeGroup{Name: "vg1"}
	key := joinKey([]string{"vgs", "--noheadings", "-o", "vg_tags", "vg1"})
	gone := errors.New("Volume group \"vg1\" not found")
	for name, call := range map[string]func(*Manager) error{
		"RemoveVolumeGroup": func(m *Manager) error { return m.RemoveVolumeGroup(context.Background(), vg) },
		"RemoveLogicalVolume": func(m *Manager) error {
			return m.RemoveLogicalVolume(context.Background(), LogicalVolume{VolumeGroup: vg, Name: "lv1"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: gone}}
			if err := call(NewManagerWithRunner(fake.run)); err != nil {
				t.Fatalf("error = %v, want nil for a group already gone", err)
			}
		})
	}
}

// Adoption writes the tag on the group and on the volumes in it, and nothing
// else: it is the one write that does not require the tag first.
func TestAdoptVolumeGroupTagsTheGroupAndItsVolumes(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}, unowned: true}
	if err := NewManagerWithRunner(fake.run).AdoptVolumeGroup(context.Background(), VolumeGroup{Name: "vg1"}); err != nil {
		t.Fatalf("AdoptVolumeGroup: %v", err)
	}
	// The volumes first and the group last, so that the group's tag is the
	// commit: an adoption that died between the two is retried whole, since the
	// group still reads as unowned.
	want := [][]string{
		{"lvchange", "--addtag", OwnerTag, "vg1"},
		{"vgchange", "--addtag", OwnerTag, "vg1"},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("issued %v, want %v", fake.calls, want)
	}
}

// The informational tags converge on exactly what was asked: a stale one goes,
// a missing one comes, the rest of the group's tags are not touched, and a
// group already right issues nothing.
func TestReconcileInformationalTags(t *testing.T) {
	vg := VolumeGroup{Name: "vg1"}
	key := joinKey([]string{"vgs", "--noheadings", "-o", "vg_tags", "vg1"})
	stale := InformationalTag("pvc", "old/claim")
	pvc := InformationalTag("pvc", "ns/claim")
	lvol := InformationalTag("lvol", "abc")
	for name, tt := range map[string]struct {
		have   string
		want   []string
		issued [][]string
	}{
		"replaces a stale claim": {
			have:   "  " + OwnerTag + "," + stale + "," + lvol + ",theirs\n",
			want:   []string{lvol, pvc},
			issued: [][]string{{"vgchange", "--deltag", stale, "vg1"}, {"vgchange", "--addtag", pvc, "vg1"}},
		},
		"already right": {
			have:   "  " + OwnerTag + "," + lvol + "," + pvc + "\n",
			want:   []string{lvol, pvc},
			issued: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRunner{out: map[string]string{key: tt.have}, err: map[string]error{}}
			if err := NewManagerWithRunner(fake.run).ReconcileInformationalTags(context.Background(), vg, tt.want); err != nil {
				t.Fatalf("ReconcileInformationalTags: %v", err)
			}
			if got := fake.mutating(); !reflect.DeepEqual(got, tt.issued) {
				t.Fatalf("issued %v, want %v", got, tt.issued)
			}
		})
	}
}

// A clone of a volume made before the tag existed is known by its names. The
// recognizer says whether the names are the driver's, and a group it does not
// recognize is refused with nothing issued against it.
func TestResolveClonedVolumeGroupRecognizesOrRefusesAnUntaggedSource(t *testing.T) {
	pvs := joinKey([]string{"pvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "vg_name", "/dev/nvme1n1"})
	owned := joinKey([]string{"pvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "vg_name,vg_tags", "/dev/nvme1n1"})
	lvsOn := joinKey([]string{"lvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "lv_name", "vdo-source"})
	lvs := joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vdo-clone1"})
	driverShaped := func(group string, volumes []string) bool { return group == "vdo-source" && len(volumes) == 2 }

	t.Run("recognized: adopted on the device, volumes and group, then imported", func(t *testing.T) {
		fake := &fakeRunner{
			out: map[string]string{pvs: "vdo-source\n", owned: "  vdo-source\n", lvsOn: "  vdopool\n  source-lv\n", lvs: "  vdopool\n  source-lv\n"},
			err: map[string]error{},
		}
		mgr := NewManagerWithRunner(fake.run)
		_, err := mgr.ResolveClonedVolumeGroup(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme1n1"},
			VolumeGroup{Name: "vdo-clone1"}, "clone1", driverShaped, "vdopool")
		if err != nil {
			t.Fatalf("ResolveClonedVolumeGroup: %v", err)
		}
		want := [][]string{
			{"pvscan", "--devices", "/dev/nvme1n1", "--cache"},
			{"lvchange", "--devices", "/dev/nvme1n1", "--addtag", OwnerTag, "vdo-source"},
			{"vgchange", "--devices", "/dev/nvme1n1", "--addtag", OwnerTag, "vdo-source"},
			{"vgimportclone", "--devices", "/dev/nvme1n1", "--basevgname", "vdo-clone1", "/dev/nvme1n1"},
			{"lvrename", "vdo-clone1", "source-lv", "clone1"},
		}
		if got := fake.mutating(); !reflect.DeepEqual(got, want) {
			t.Fatalf("issued %v, want %v", got, want)
		}
	})

	t.Run("not recognized: refused, nothing issued", func(t *testing.T) {
		fake := &fakeRunner{
			out: map[string]string{pvs: "vdo-source\n", owned: "  vdo-source\n", lvsOn: "  data\n"},
			err: map[string]error{}, unowned: true,
		}
		_, err := NewManagerWithRunner(fake.run).ResolveClonedVolumeGroup(context.Background(),
			PhysicalVolume{DevicePath: "/dev/nvme1n1"}, VolumeGroup{Name: "vdo-clone1"}, "clone1", driverShaped, "vdopool")
		if !errors.Is(err, ErrNotOwned) {
			t.Fatalf("error = %v, want ErrNotOwned", err)
		}
		for _, call := range fake.mutating() {
			if call[0] != "pvscan" {
				t.Fatalf("the refusal issued %v", call)
			}
		}
	})
}

// LVM prints its notices ahead of a report's values and on the same stream, and
// the harness's own lvm.conf makes it print two of them before every listing. A
// reading that took the first line would take the notice for the value, which
// on a tags listing reads as a group that is not the driver's.
func TestListingsSkipTheNoticesLVMPrintsFirst(t *testing.T) {
	vg := VolumeGroup{Name: "vg1"}
	key := joinKey([]string{"vgs", "--noheadings", "-o", "vg_tags", "vg1"})
	out := "Please remove the lvm.conf global_filter, it is ignored with the devices file.\n" +
		"  Please remove the lvm.conf filter, it is ignored with the devices file.\n" +
		"  " + OwnerTag + "\n"
	fake := &fakeRunner{out: map[string]string{key: out}, err: map[string]error{}}
	got, err := NewManagerWithRunner(fake.run).VolumeGroupTags(context.Background(), vg)
	if err != nil {
		t.Fatalf("VolumeGroupTags: %v", err)
	}
	if !reflect.DeepEqual(got, []string{OwnerTag}) {
		t.Fatalf("tags = %v, want only %s", got, OwnerTag)
	}
}
