package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestManager_ImportClonedVolumeGroup(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	vg := VolumeGroup{Name: "vdo-clone1"}
	pv := PhysicalVolume{DevicePath: "/dev/nvme1n1"}
	if err := mgr.ImportClonedVolumeGroup(context.Background(), vg, pv); err != nil {
		t.Fatalf("ImportClonedVolumeGroup: %v", err)
	}
	want := []string{"vgimportclone", "--devices", "/dev/nvme1n1", "--basevgname", "vdo-clone1", "/dev/nvme1n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_ImportClonedVolumeGroup_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("device or resource busy")
	key := joinKey([]string{"vgimportclone", "--devices", "/dev/nvme1n1", "--basevgname", "vdo-clone1", "/dev/nvme1n1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	vg := VolumeGroup{Name: "vdo-clone1"}
	pv := PhysicalVolume{DevicePath: "/dev/nvme1n1"}
	err := mgr.ImportClonedVolumeGroup(context.Background(), vg, pv)
	if !errors.Is(err, wantErr) {
		t.Errorf("ImportClonedVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_RenameLogicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	vg := VolumeGroup{Name: "vdo-clone1"}
	err := mgr.RenameLogicalVolume(context.Background(), vg, "source-lv", "clone1")
	if err != nil {
		t.Fatalf("RenameLogicalVolume: %v", err)
	}
	want := []string{"lvrename", "vdo-clone1", "source-lv", "clone1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

// The composite's whole value is the order and the conditions, so these tests
// assert the recorded command sequence, not just individual calls.
func TestManager_ResolveClonedVolumeGroup_ResolvesAForeignIdentity(t *testing.T) {
	pvs := joinKey([]string{
		"pvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "vg_name", "/dev/nvme1n1",
	})
	lvs := joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vdo-clone1"})
	fake := &fakeRunner{
		out: map[string]string{pvs: "vdo-source\n", lvs: "  vdopool\n  source-lv\n"},
		err: map[string]error{},
	}
	mgr := NewManagerWithRunner(fake.run)

	pv := PhysicalVolume{DevicePath: "/dev/nvme1n1"}
	vg := VolumeGroup{Name: "vdo-clone1"}
	previous, err := mgr.ResolveClonedVolumeGroup(context.Background(), pv, vg, "clone1", "vdopool")
	if err != nil {
		t.Fatalf("ResolveClonedVolumeGroup: %v", err)
	}
	if want := (VolumeGroup{Name: "vdo-source"}); previous != want {
		t.Errorf("previous VG = %v, want %v", previous, want)
	}
	want := [][]string{
		{"pvscan", "--devices", "/dev/nvme1n1", "--cache"},
		{"pvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "vg_name", "/dev/nvme1n1"},
		{"vgimportclone", "--devices", "/dev/nvme1n1", "--basevgname", "vdo-clone1", "/dev/nvme1n1"},
		{"lvs", "--noheadings", "-o", "lv_name", "vdo-clone1"},
		{"lvrename", "vdo-clone1", "source-lv", "clone1"},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("recorded calls =\n%v\nwant\n%v", fake.calls, want)
	}
}

// A device already carrying this volume's own identity, and a blank one, are
// both left completely alone: no import, no rename.
func TestManager_ResolveClonedVolumeGroup_NoOps(t *testing.T) {
	pvs := joinKey([]string{
		"pvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "vg_name", "/dev/nvme1n1",
	})
	tests := []struct {
		name string
		out  string
	}{
		{"already this volume's identity", "vdo-clone1\n"},
		{"blank device", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRunner{out: map[string]string{pvs: tt.out}, err: map[string]error{}}
			mgr := NewManagerWithRunner(fake.run)
			pv := PhysicalVolume{DevicePath: "/dev/nvme1n1"}
			vg := VolumeGroup{Name: "vdo-clone1"}
			previous, err := mgr.ResolveClonedVolumeGroup(context.Background(), pv, vg, "clone1", "vdopool")
			if err != nil {
				t.Fatalf("ResolveClonedVolumeGroup: %v", err)
			}
			if previous != (VolumeGroup{}) {
				t.Errorf("previous VG = %v, want the zero value (nothing resolved)", previous)
			}
			for _, call := range fake.calls {
				if call[0] == "vgimportclone" || call[0] == "lvrename" {
					t.Errorf("ran %v against a device that needed no resolution", call)
				}
			}
		})
	}
}

// The structural LV a stack names identically in every volume (VDO's pool) must
// survive: renaming it would break the stack the clone is supposed to become.
func TestManager_ResolveClonedVolumeGroup_PreservesStructuralLVs(t *testing.T) {
	pvs := joinKey([]string{
		"pvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "vg_name", "/dev/nvme1n1",
	})
	lvs := joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vdo-clone1"})
	fake := &fakeRunner{
		out: map[string]string{pvs: "vdo-source\n", lvs: "  vdopool\n"},
		err: map[string]error{},
	}
	mgr := NewManagerWithRunner(fake.run)

	pv := PhysicalVolume{DevicePath: "/dev/nvme1n1"}
	vg := VolumeGroup{Name: "vdo-clone1"}
	if _, err := mgr.ResolveClonedVolumeGroup(context.Background(), pv, vg, "clone1", "vdopool"); err != nil {
		t.Fatalf("ResolveClonedVolumeGroup: %v", err)
	}
	for _, call := range fake.calls {
		if call[0] == "lvrename" {
			t.Errorf("renamed the preserved pool LV: %v", call)
		}
	}
}

// A failed cache refresh is not fatal: the content probe that follows reads the
// device directly.
func TestManager_ResolveClonedVolumeGroup_SurvivesAFailedRescan(t *testing.T) {
	pvs := joinKey([]string{
		"pvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "vg_name", "/dev/nvme1n1",
	})
	fake := &fakeRunner{
		out: map[string]string{pvs: "vdo-clone1\n"},
		err: map[string]error{
			joinKey([]string{"pvscan", "--devices", "/dev/nvme1n1", "--cache"}): errors.New("pvscan failed"),
		},
	}
	mgr := NewManagerWithRunner(fake.run)
	pv := PhysicalVolume{DevicePath: "/dev/nvme1n1"}
	vg := VolumeGroup{Name: "vdo-clone1"}
	if _, err := mgr.ResolveClonedVolumeGroup(context.Background(), pv, vg, "clone1"); err != nil {
		t.Errorf("ResolveClonedVolumeGroup: %v, want the failed pvscan to be non-fatal", err)
	}
}

func TestManager_ResolveClonedVolumeGroup_WrapsAProbeFailure(t *testing.T) {
	wantErr := errors.New("input/output error")
	pvs := joinKey([]string{
		"pvs", "--devices", "/dev/nvme1n1", "--noheadings", "-o", "vg_name", "/dev/nvme1n1",
	})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{pvs: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	pv := PhysicalVolume{DevicePath: "/dev/nvme1n1"}
	vg := VolumeGroup{Name: "vdo-clone1"}
	_, err := mgr.ResolveClonedVolumeGroup(context.Background(), pv, vg, "clone1")
	if !errors.Is(err, wantErr) {
		t.Errorf("ResolveClonedVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}
