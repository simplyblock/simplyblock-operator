package lvm

import (
	"context"
	"errors"
	"testing"
)

func TestManager_ForgetDevice(t *testing.T) {
	t.Run("removes the device's entry", func(t *testing.T) {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)

		pv := PhysicalVolume{DevicePath: "/dev/nvme0n1"}
		if err := mgr.ForgetDevice(context.Background(), pv); err != nil {
			t.Fatalf("ForgetDevice: %v", err)
		}

		key := joinKey([]string{"lvmdevices", "--devices", pv.DevicePath, "--deldev", pv.DevicePath})
		if fake.out[key] != "" && len(fake.calls) == 0 {
			t.Fatal("lvmdevices --deldev was never run")
		}
		found := false
		for _, call := range fake.calls {
			if joinKey(call) == key {
				found = true
			}
		}
		if !found {
			t.Errorf("expected call %q, got calls %v", key, fake.calls)
		}
	})

	t.Run("wraps the command's error", func(t *testing.T) {
		pv := PhysicalVolume{DevicePath: "/dev/nvme0n1"}
		wantErr := errors.New("device not found in devices file")
		fake := &fakeRunner{
			out: map[string]string{},
			err: map[string]error{
				joinKey([]string{"lvmdevices", "--devices", pv.DevicePath, "--deldev", pv.DevicePath}): wantErr,
			},
		}
		mgr := NewManagerWithRunner(fake.run)

		if err := mgr.ForgetDevice(context.Background(), pv); !errors.Is(err, wantErr) {
			t.Errorf("ForgetDevice() error = %v, want wrapping %v", err, wantErr)
		}
	})
}
