package lvm

import (
	"context"
	"reflect"
	"testing"
)

// fakeRunner records every command line it's asked to run and answers from a
// script keyed by the joined args, so no lvm2 binary has to be present.
// Shared by every test file in this package.
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
	s := ""
	for _, a := range args {
		s += a + " "
	}
	return s
}

func TestDeviceScope(t *testing.T) {
	tests := []struct {
		name    string
		devices []string
		want    []string
	}{
		{"single device", []string{"/dev/nvme0n1"}, []string{"--devices", "/dev/nvme0n1"}},
		{
			"multiple devices, comma-joined",
			[]string{"/dev/nvme0n1", "/dev/nvme1n1"},
			[]string{"--devices", "/dev/nvme0n1,/dev/nvme1n1"},
		},
		{"no devices, runs unscoped", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeviceScope(tt.devices...); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DeviceScope(%v) = %v, want %v", tt.devices, got, tt.want)
			}
		})
	}
}

func TestManager_Run_InsertsDeviceScopeAfterBinary(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	_, err := mgr.Run(context.Background(), []string{"/dev/nvme0n1"}, "pvcreate", "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"pvcreate", "--devices", "/dev/nvme0n1", "/dev/nvme0n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_Run_NoDevicesRunsUnscoped(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	_, err := mgr.Run(context.Background(), nil, "vgchange", "-an", "vdo-abc123")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"vgchange", "-an", "vdo-abc123"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v (no --devices flag)", fake.calls, want)
	}
}

func TestManager_Run_RequiresACommandName(t *testing.T) {
	mgr := NewManagerWithRunner(nil)
	if _, err := mgr.Run(context.Background(), []string{"/dev/nvme0n1"}); err == nil {
		t.Error("expected an error for a call with no command name")
	}
}
