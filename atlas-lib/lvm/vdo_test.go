package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestManager_CreateVDOLogicalVolume(t *testing.T) {
	tests := []struct {
		name                       string
		compression, deduplication bool
		wantCompression, wantDedup string
	}{
		{"both on", true, true, "y", "y"},
		{"compression only", true, false, "y", "n"},
		{"dedup only", false, true, "n", "y"},
		{"both off", false, false, "n", "n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
			mgr := NewManagerWithRunner(fake.run)
			err := mgr.CreateVDOLogicalVolume(
				context.Background(), []string{"/dev/nvme0n1"}, "vdo-abc123", "vdopool", "abc123",
				tt.compression, tt.deduplication,
			)
			if err != nil {
				t.Fatalf("CreateVDOLogicalVolume: %v", err)
			}
			want := []string{
				"lvcreate", "--devices", "/dev/nvme0n1",
				"--type", "vdo",
				"--config", "activation{checks=0}",
				"-n", "abc123",
				"-l", "100%FREE",
				"--compression", tt.wantCompression,
				"--deduplication", tt.wantDedup,
				"vdo-abc123/vdopool",
				"--yes",
			}
			if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
				t.Errorf("recorded call = %v, want %v", fake.calls, want)
			}
		})
	}
}

func TestManager_CreateVDOLogicalVolume_WrapsRunnerError(t *testing.T) {
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{
			joinKey([]string{
				"lvcreate", "--devices", "/dev/nvme0n1",
				"--type", "vdo", "--config", "activation{checks=0}",
				"-n", "abc123", "-l", "100%FREE",
				"--compression", "y", "--deduplication", "y",
				"vdo-abc123/vdopool", "--yes",
			}): errors.New("Not enough free memory for VDO target"),
		},
	}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.CreateVDOLogicalVolume(
		context.Background(), []string{"/dev/nvme0n1"}, "vdo-abc123", "vdopool", "abc123", true, true,
	)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestManager_SetVDOFeatures(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.SetVDOFeatures(context.Background(), nil, "vdo-abc123", "vdopool", true, false); err != nil {
		t.Fatalf("SetVDOFeatures: %v", err)
	}
	want := []string{"lvchange", "--compression", "y", "--deduplication", "n", "vdo-abc123/vdopool"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}
