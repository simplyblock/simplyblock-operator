package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestManager_VolumeGroup(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"blank device", "", nil, ""},
		{"belongs to a VG", "  vdo-abc123  \n", nil, "vdo-abc123"},
		{
			"WARNING lines ahead of the real field, from a duplicate-PV clone",
			"WARNING: PV a is duplicate for PVID b\n  vdo-abc123\n",
			nil,
			"vdo-abc123",
		},
		{"no PV signature on the device", "", errors.New(`Failed to find physical volume "/dev/nvme0n1"`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name", "/dev/nvme0n1"})
			fake := &fakeRunner{
				out: map[string]string{key: tt.out},
				err: map[string]error{key: tt.err},
			}
			mgr := NewManagerWithRunner(fake.run)
			got, err := mgr.VolumeGroup(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"})
			if err != nil {
				t.Fatalf("VolumeGroup: %v", err)
			}
			if got.Name != tt.want {
				t.Errorf("VolumeGroup() = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestManager_VolumeGroup_PropagatesRealProbeError(t *testing.T) {
	wantErr := errors.New("device or resource busy")
	key := joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name", "/dev/nvme0n1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	if _, err := mgr.VolumeGroup(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"}); !errors.Is(err, wantErr) {
		t.Errorf("VolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_ListLogicalVolumes(t *testing.T) {
	key := joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vg1"})
	fake := &fakeRunner{
		out: map[string]string{key: "  vdopool\n  data1\n"},
		err: map[string]error{},
	}
	mgr := NewManagerWithRunner(fake.run)
	vg := VolumeGroup{Name: "vg1"}
	got, err := mgr.ListLogicalVolumes(context.Background(), vg)
	if err != nil {
		t.Fatalf("ListLogicalVolumes: %v", err)
	}
	want := []LogicalVolume{{VolumeGroup: vg, Name: "vdopool"}, {VolumeGroup: vg, Name: "data1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListLogicalVolumes() = %v, want %v", got, want)
	}
}

func TestManager_ListLogicalVolumes_PropagatesRunnerError(t *testing.T) {
	wantErr := errors.New("failed to find VG")
	key := joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vg1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	if _, err := mgr.ListLogicalVolumes(context.Background(), VolumeGroup{Name: "vg1"}); !errors.Is(err, wantErr) {
		t.Errorf("ListLogicalVolumes() error = %v, want %v", err, wantErr)
	}
}

func TestManager_HasLogicalVolume(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"lv present among others", "  poolvol\n  data1\n", nil, true},
		{"orphaned VG, zero LVs", "", nil, false},
		{"lv absent", "  poolvol\n", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vg1"})
			fake := &fakeRunner{
				out: map[string]string{key: tt.out},
				err: map[string]error{key: tt.err},
			}
			mgr := NewManagerWithRunner(fake.run)
			lv := LogicalVolume{VolumeGroup: VolumeGroup{Name: "vg1"}, Name: "data1"}
			got, err := mgr.HasLogicalVolume(context.Background(), lv)
			if err != nil {
				t.Fatalf("HasLogicalVolume: %v", err)
			}
			if got != tt.want {
				t.Errorf("HasLogicalVolume() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManager_HasLogicalVolume_PropagatesRunnerError(t *testing.T) {
	wantErr := errors.New("failed to find VG")
	key := joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vg1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	lv := LogicalVolume{VolumeGroup: VolumeGroup{Name: "vg1"}, Name: "data1"}
	if _, err := mgr.HasLogicalVolume(context.Background(), lv); !errors.Is(err, wantErr) {
		t.Errorf("HasLogicalVolume() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_Rescan(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	err := mgr.Rescan(context.Background(),
		PhysicalVolume{DevicePath: "/dev/nvme0n1"}, PhysicalVolume{DevicePath: "/dev/nvme1n1"},
	)
	if err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	want := []string{"pvscan", "--devices", "/dev/nvme0n1,/dev/nvme1n1", "--cache"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_Rescan_PropagatesRunnerError(t *testing.T) {
	wantErr := errors.New("lock contention")
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{joinKey([]string{"pvscan", "--devices", "/dev/nvme0n1", "--cache"}): wantErr},
	}
	mgr := NewManagerWithRunner(fake.run)

	if err := mgr.Rescan(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"}); !errors.Is(err, wantErr) {
		t.Errorf("Rescan() error = %v, want %v", err, wantErr)
	}
}
