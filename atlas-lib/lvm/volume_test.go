package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestManager_CreatePhysicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	if err := mgr.CreatePhysicalVolume(context.Background(), "/dev/nvme0n1"); err != nil {
		t.Fatalf("CreatePhysicalVolume: %v", err)
	}
	want := []string{"pvcreate", "--devices", "/dev/nvme0n1", "/dev/nvme0n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_CreatePhysicalVolume_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("device is not empty")
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{joinKey([]string{"pvcreate", "--devices", "/dev/nvme0n1", "/dev/nvme0n1"}): wantErr},
	}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.CreatePhysicalVolume(context.Background(), "/dev/nvme0n1"); !errors.Is(err, wantErr) {
		t.Errorf("CreatePhysicalVolume() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_CreateVolumeGroup(t *testing.T) {
	t.Run("single device", func(t *testing.T) {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)
		if err := mgr.CreateVolumeGroup(context.Background(), "vg1", "/dev/nvme0n1"); err != nil {
			t.Fatalf("CreateVolumeGroup: %v", err)
		}
		want := []string{"vgcreate", "--devices", "/dev/nvme0n1", "vg1", "/dev/nvme0n1"}
		if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
			t.Errorf("recorded call = %v, want %v", fake.calls, want)
		}
	})

	t.Run("multiple devices, for a striped VG", func(t *testing.T) {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)
		devices := []string{"/dev/nvme0n1", "/dev/nvme1n1"}
		if err := mgr.CreateVolumeGroup(context.Background(), "vg1", devices...); err != nil {
			t.Fatalf("CreateVolumeGroup: %v", err)
		}
		want := []string{
			"vgcreate", "--devices", "/dev/nvme0n1,/dev/nvme1n1", "vg1", "/dev/nvme0n1", "/dev/nvme1n1",
		}
		if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
			t.Errorf("recorded call = %v, want %v", fake.calls, want)
		}
	})
}

func TestManager_ActivateVolumeGroup(t *testing.T) {
	t.Run("scoped", func(t *testing.T) {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)
		if err := mgr.ActivateVolumeGroup(context.Background(), "vg1"); err != nil {
			t.Fatalf("ActivateVolumeGroup: %v", err)
		}
		want := []string{"vgchange", "-ay", "vg1"}
		if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
			t.Errorf("recorded call = %v, want %v", fake.calls, want)
		}
	})

	t.Run("unscoped, by name alone", func(t *testing.T) {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)
		if err := mgr.ActivateVolumeGroup(context.Background(), "vg1"); err != nil {
			t.Fatalf("ActivateVolumeGroup: %v", err)
		}
		want := []string{"vgchange", "-ay", "vg1"}
		if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
			t.Errorf("recorded call = %v, want %v", fake.calls, want)
		}
	})
}

func TestManager_DeactivateVolumeGroup(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.DeactivateVolumeGroup(context.Background(), "vg1"); err != nil {
		t.Fatalf("DeactivateVolumeGroup: %v", err)
	}
	want := []string{"vgchange", "-an", "vg1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_DeactivateVolumeGroup_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("device busy")
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{joinKey([]string{"vgchange", "-an", "vg1"}): wantErr},
	}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.DeactivateVolumeGroup(context.Background(), "vg1"); !errors.Is(err, wantErr) {
		t.Errorf("DeactivateVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_RemoveVolumeGroup(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.RemoveVolumeGroup(context.Background(), "vg1"); err != nil {
		t.Fatalf("RemoveVolumeGroup: %v", err)
	}
	want := []string{"vgremove", "-f", "vg1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_RemoveVolumeGroup_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("volume group not found")
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{joinKey([]string{"vgremove", "-f", "vg1"}): wantErr},
	}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.RemoveVolumeGroup(context.Background(), "vg1"); !errors.Is(err, wantErr) {
		t.Errorf("RemoveVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}
