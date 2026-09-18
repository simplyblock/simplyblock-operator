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

func TestManager_ForgetDevice_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("device not found")
	key := joinKey([]string{"lvmdevices", "--devices", "/dev/nvme1n1", "--deldev", "/dev/nvme1n1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)
	err := mgr.ForgetDevice(context.Background(), "/dev/nvme1n1")
	if !errors.Is(err, wantErr) {
		t.Errorf("ForgetDevice() error = %v, want wrapping %v", err, wantErr)
	}
}
