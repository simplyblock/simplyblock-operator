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
	if err := mgr.ImportClonedVolumeGroup(context.Background(), []string{"/dev/nvme1n1"}, "vdo-clone1", "/dev/nvme1n1"); err != nil {
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
	err := mgr.ImportClonedVolumeGroup(context.Background(), []string{"/dev/nvme1n1"}, "vdo-clone1", "/dev/nvme1n1")
	if !errors.Is(err, wantErr) {
		t.Errorf("ImportClonedVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_RenameLogicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.RenameLogicalVolume(context.Background(), []string{"/dev/nvme1n1"}, "vdo-clone1", "source-lv", "clone1")
	if err != nil {
		t.Fatalf("RenameLogicalVolume: %v", err)
	}
	want := []string{"lvrename", "--devices", "/dev/nvme1n1", "vdo-clone1", "source-lv", "clone1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}
