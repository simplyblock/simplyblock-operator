package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestManager_ForgetDevice(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.ForgetDevice(context.Background(), "/dev/nvme1n1"); err != nil {
		t.Fatalf("ForgetDevice: %v", err)
	}
	want := []string{"lvmdevices", "--devices", "/dev/nvme1n1", "--deldev", "/dev/nvme1n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

// A device the file does not list is already in the state the caller asked for,
// and this is the common answer rather than an unusual one: nothing adds an
// entry for a device that was never labeled.
func TestManager_ForgetDevice_IsANoOpWhenAlreadyGone(t *testing.T) {
	key := joinKey([]string{"lvmdevices", "--devices", "/dev/nvme1n1", "--deldev", "/dev/nvme1n1"})
	for _, gone := range []string{"device not found", "Failed to find device /dev/nvme1n1"} {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: errors.New(gone)}}
		mgr := NewManagerWithRunner(fake.run)
		if err := mgr.ForgetDevice(context.Background(), "/dev/nvme1n1"); err != nil {
			t.Errorf("ForgetDevice() on %q = %v, want nil: it is already gone", gone, err)
		}
	}
}

// Any other failure still says so: "already gone" is one specific answer, not a
// reason to swallow a devices file that could not be written at all.
func TestManager_ForgetDevice_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("permission denied")
	key := joinKey([]string{"lvmdevices", "--devices", "/dev/nvme1n1", "--deldev", "/dev/nvme1n1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ForgetDevice(context.Background(), "/dev/nvme1n1")
	if !errors.Is(err, wantErr) {
		t.Errorf("ForgetDevice() error = %v, want wrapping %v", err, wantErr)
	}
}
