package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestManager_ExpandPhysicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.ExpandPhysicalVolume(context.Background(), "/dev/nvme0n1"); err != nil {
		t.Fatalf("ExpandPhysicalVolume: %v", err)
	}
	want := []string{"pvresize", "/dev/nvme0n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_ExtendVolumeGroup(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.ExtendVolumeGroup(context.Background(), "striped-vg", "/dev/nvme1n1"); err != nil {
		t.Fatalf("ExtendVolumeGroup: %v", err)
	}
	want := []string{"vgextend", "striped-vg", "/dev/nvme1n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_ExtendVolumeGroup_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("insufficient free extents")
	key := joinKey([]string{"vgextend", "striped-vg", "/dev/nvme1n1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ExtendVolumeGroup(context.Background(), "striped-vg", "/dev/nvme1n1")
	if !errors.Is(err, wantErr) {
		t.Errorf("ExtendVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_ExpandLogicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ExpandLogicalVolume(context.Background(), "vdo-abc123", "vdopool")
	if err != nil {
		t.Fatalf("ExpandLogicalVolume: %v", err)
	}
	// The "+" prefix is what makes this additive rather than an absolute target. See the
	// method's doc comment for why an unprefixed 100%FREE would be a real bug here.
	want := []string{"lvextend", "-l+100%FREE", "vdo-abc123/vdopool"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_LogicalVolumeSize(t *testing.T) {
	key := joinKey([]string{
		"lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size",
		"vdo-abc123/vdopool",
	})
	fake := &fakeRunner{out: map[string]string{key: "  8589934592\n"}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	got, err := mgr.LogicalVolumeSize(context.Background(), "vdo-abc123", "vdopool")
	if err != nil {
		t.Fatalf("LogicalVolumeSize: %v", err)
	}
	if got != 8589934592 {
		t.Errorf("LogicalVolumeSize() = %d, want 8589934592", got)
	}
}

func TestManager_LogicalVolumeSize_UnparsableOutput(t *testing.T) {
	key := joinKey([]string{
		"lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size",
		"vdo-abc123/vdopool",
	})
	fake := &fakeRunner{out: map[string]string{key: "not a number"}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if _, err := mgr.LogicalVolumeSize(context.Background(), "vdo-abc123", "vdopool"); err == nil {
		t.Error("expected an error for unparsable lvs output")
	}
}

func TestManager_ExtendLogicalVolumeToSize(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ExtendLogicalVolumeToSize(context.Background(), "vdo-abc123", "abc123", 8589934592)
	if err != nil {
		t.Fatalf("ExtendLogicalVolumeToSize: %v", err)
	}
	want := []string{"lvextend", "-L8589934592B", "vdo-abc123/abc123"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_ExtendLogicalVolumeToSize_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("insufficient free extents")
	key := joinKey([]string{"lvextend", "-L100B", "vdo-abc123/abc123"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ExtendLogicalVolumeToSize(context.Background(), "vdo-abc123", "abc123", 100)
	if !errors.Is(err, wantErr) {
		t.Errorf("ExtendLogicalVolumeToSize() error = %v, want wrapping %v", err, wantErr)
	}
}
