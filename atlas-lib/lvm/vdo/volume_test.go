// Unit tests for the VDO volume helpers: they assert the exact lvcreate and
// lvchange command lines the package builds, since a wrong flag or a wrong
// vg/lv path is the failure mode that only surfaces on a live node with dm-vdo
// loaded. Every test drives a real lvm.Manager whose command execution is
// supplied by the fake below, so no lvm2 binary or kernel module is needed.
package vdo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/lvm"
)

// fakeRunner records every command line it's asked to run and answers from a
// script keyed by the joined args. It is handed to lvm.NewManagerWithRunner as
// a method value: that package's runner type is unexported, but a matching
// func value is assignable to it from outside the package.
type fakeRunner struct {
	calls [][]string
	out   map[string]string
	err   map[string]error
}

func (f *fakeRunner) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	key := joinKey(args)
	return f.out[key], f.err[key]
}

func joinKey(args []string) string {
	return strings.Join(args, " ") + " "
}

// newManager returns a Manager wired to fake, plus the fake itself, which is
// the setup every test here shares.
func newManager() (*lvm.Manager, *fakeRunner) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	return lvm.NewManagerWithRunner(fake.run), fake
}

func TestNewVolume(t *testing.T) {
	tests := []struct {
		name                       string
		compression, deduplication bool
		wantCompression, wantDedup string
	}{
		{"both on", true, true, "y", "y"},
		{"compression only", true, false, "y", "n"},
		{"deduplication only", false, true, "n", "y"},
		{"both off", false, false, "n", "n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, fake := newManager()
			volume, err := NewVolume(
				context.Background(), mgr, []string{"/dev/nvme0n1"}, "vdo-abc123", "vdopool", "abc123",
				tt.compression, tt.deduplication,
			)
			if err != nil {
				t.Fatalf("NewVolume: %v", err)
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
			if volume == nil {
				t.Fatal("NewVolume returned a nil volume without an error")
			}
			if volume.volumeGroupName != "vdo-abc123" || volume.poolName != "vdopool" ||
				volume.logicalVolumeName != "abc123" {
				t.Errorf(
					"volume identity = %s/%s/%s, want vdo-abc123/vdopool/abc123",
					volume.volumeGroupName, volume.poolName, volume.logicalVolumeName,
				)
			}
		})
	}
}

// A volume group addressed by name alone runs unscoped: no --devices flag at
// all, rather than an empty one LVM would reject.
func TestNewVolume_NoDevicesRunsUnscoped(t *testing.T) {
	mgr, fake := newManager()
	if _, err := NewVolume(
		context.Background(), mgr, nil, "vdo-abc123", "vdopool", "abc123", true, true,
	); err != nil {
		t.Fatalf("NewVolume: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(fake.calls))
	}
	for _, arg := range fake.calls[0] {
		if arg == "--devices" {
			t.Errorf("recorded call = %v, want no --devices flag", fake.calls[0])
		}
	}
}

func TestNewVolume_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("Not enough free memory for VDO target")
	key := joinKey([]string{
		"lvcreate", "--devices", "/dev/nvme0n1",
		"--type", "vdo", "--config", "activation{checks=0}",
		"-n", "abc123", "-l", "100%FREE",
		"--compression", "y", "--deduplication", "y",
		"vdo-abc123/vdopool", "--yes",
	})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	volume, err := NewVolume(
		context.Background(), lvm.NewManagerWithRunner(fake.run), []string{"/dev/nvme0n1"},
		"vdo-abc123", "vdopool", "abc123", true, true,
	)
	if !errors.Is(err, wantErr) {
		t.Errorf("NewVolume() error = %v, want wrapping %v", err, wantErr)
	}
	if volume != nil {
		t.Errorf("NewVolume() volume = %+v, want nil on error", volume)
	}
}

// UpdateVolume addresses the VDO pool the volume was created on, which is
// where lvm2 keeps the compression and deduplication attributes, and runs
// unscoped: the pool is reached by name once the stack is assembled.
func TestUpdateVolume(t *testing.T) {
	mgr, fake := newManager()
	volume, err := NewVolume(
		context.Background(), mgr, []string{"/dev/nvme0n1"}, "vdo-abc123", "vdopool", "abc123", true, true,
	)
	if err != nil {
		t.Fatalf("NewVolume: %v", err)
	}
	if err := UpdateVolume(context.Background(), mgr, volume, true, false); err != nil {
		t.Fatalf("UpdateVolume: %v", err)
	}
	want := []string{"lvchange", "--compression", "y", "--deduplication", "n", "vdo-abc123/vdopool"}
	if len(fake.calls) != 2 || !reflect.DeepEqual(fake.calls[1], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls[1:], want)
	}
}

func TestUpdateVolume_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("VDO pool is not active")
	key := joinKey([]string{"lvchange", "--compression", "n", "--deduplication", "n", "vdo-abc123/vdopool"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	volume := &Volume{volumeGroupName: "vdo-abc123", poolName: "vdopool", logicalVolumeName: "abc123"}
	err := UpdateVolume(context.Background(), lvm.NewManagerWithRunner(fake.run), volume, false, false)
	if !errors.Is(err, wantErr) {
		t.Errorf("UpdateVolume() error = %v, want wrapping %v", err, wantErr)
	}
}
