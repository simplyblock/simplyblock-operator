package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestManager_ExtendPhysicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.ExtendPhysicalVolume(context.Background(), []string{"/dev/nvme0n1"}, "/dev/nvme0n1"); err != nil {
		t.Fatalf("ExtendPhysicalVolume: %v", err)
	}
	want := []string{"pvresize", "--devices", "/dev/nvme0n1", "/dev/nvme0n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_ExtendVolumeGroup(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	devices := []string{"/dev/nvme0n1", "/dev/nvme1n1"}
	if err := mgr.ExtendVolumeGroup(context.Background(), devices, "striped-vg", "/dev/nvme1n1"); err != nil {
		t.Fatalf("ExtendVolumeGroup: %v", err)
	}
	want := []string{"vgextend", "--devices", "/dev/nvme0n1,/dev/nvme1n1", "striped-vg", "/dev/nvme1n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_ExtendVolumeGroup_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("insufficient free extents")
	key := joinKey([]string{"vgextend", "striped-vg", "/dev/nvme1n1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ExtendVolumeGroup(context.Background(), nil, "striped-vg", "/dev/nvme1n1")
	if !errors.Is(err, wantErr) {
		t.Errorf("ExtendVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_ExtendLogicalVolumeByFreeSpace(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ExtendLogicalVolumeByFreeSpace(context.Background(), []string{"/dev/nvme0n1"}, "vdo-abc123", "vdopool")
	if err != nil {
		t.Fatalf("ExtendLogicalVolumeByFreeSpace: %v", err)
	}
	// The "+" prefix is what makes this additive rather than an absolute target. See the
	// method's doc comment for why an unprefixed 100%FREE would be a real bug here.
	want := []string{"lvextend", "--devices", "/dev/nvme0n1", "-l+100%FREE", "vdo-abc123/vdopool"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_LogicalVolumeSize(t *testing.T) {
	key := joinKey([]string{
		"lvs", "--devices", "/dev/nvme0n1", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size",
		"vdo-abc123/vdopool",
	})
	fake := &fakeRunner{out: map[string]string{key: "  8589934592\n"}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	got, err := mgr.LogicalVolumeSize(context.Background(), []string{"/dev/nvme0n1"}, "vdo-abc123", "vdopool")
	if err != nil {
		t.Fatalf("LogicalVolumeSize: %v", err)
	}
	if got != 8589934592 {
		t.Errorf("LogicalVolumeSize() = %d, want 8589934592", got)
	}
}

func TestManager_LogicalVolumeSize_UnparsableOutput(t *testing.T) {
	key := joinKey([]string{
		"lvs", "--devices", "/dev/nvme0n1", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size",
		"vdo-abc123/vdopool",
	})
	fake := &fakeRunner{out: map[string]string{key: "not a number"}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if _, err := mgr.LogicalVolumeSize(context.Background(), []string{"/dev/nvme0n1"}, "vdo-abc123", "vdopool"); err == nil {
		t.Error("expected an error for unparsable lvs output")
	}
}

func TestManager_ExtendLogicalVolumeToSize(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ExtendLogicalVolumeToSize(context.Background(), []string{"/dev/nvme0n1"}, "vdo-abc123", "abc123", 8589934592)
	if err != nil {
		t.Fatalf("ExtendLogicalVolumeToSize: %v", err)
	}
	want := []string{"lvextend", "--devices", "/dev/nvme0n1", "-L8589934592B", "vdo-abc123/abc123"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_ExtendLogicalVolumeToSize_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("insufficient free extents")
	key := joinKey([]string{"lvextend", "--devices", "/dev/nvme0n1", "-L100B", "vdo-abc123/abc123"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ExtendLogicalVolumeToSize(context.Background(), []string{"/dev/nvme0n1"}, "vdo-abc123", "abc123", 100)
	if !errors.Is(err, wantErr) {
		t.Errorf("ExtendLogicalVolumeToSize() error = %v, want wrapping %v", err, wantErr)
	}
}
