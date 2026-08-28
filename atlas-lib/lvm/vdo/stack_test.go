// Unit tests for the VDO stack lifecycle (CreateOrAttach, ResolveClone,
// Deactivate, Remove, Grow, SetFeatures): each asserts the exact sequence of
// LVM commands the function issues, since a wrong order or a wrong scope is
// the failure mode that only surfaces on a live node with dm-vdo loaded.
package vdo

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/simplyblock/atlas/lvm"
)

func TestDevicePath(t *testing.T) {
	if got, want := DevicePath("abc123"), "/dev/vdo-abc123/abc123"; got != want {
		t.Errorf("DevicePath() = %q, want %q", got, want)
	}
}

func TestCreateOrAttach_FreshDevice(t *testing.T) {
	mgr, fake := newManager()
	got, err := CreateOrAttach(context.Background(), mgr, "/dev/nvme0n1", "abc123", true, false)
	if err != nil {
		t.Fatalf("CreateOrAttach: %v", err)
	}
	if want := "/dev/vdo-abc123/abc123"; got != want {
		t.Errorf("CreateOrAttach() = %q, want %q", got, want)
	}
	want := [][]string{
		{"pvscan", "--devices", "/dev/nvme0n1", "--cache"},
		{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name", "/dev/nvme0n1"},
		{"pvcreate", "--devices", "/dev/nvme0n1", "/dev/nvme0n1"},
		{"vgcreate", "--devices", "/dev/nvme0n1", "vdo-abc123", "/dev/nvme0n1"},
		{
			"lvcreate", "-n", "abc123", "-l", "100%FREE", "vdo-abc123/vdopool", "--yes",
			"--type", "vdo", "--config", "activation{checks=0}", "--compression", "y", "--deduplication", "n",
		},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("recorded calls = %v, want %v", fake.calls, want)
	}
}

func TestCreateOrAttach_ExistingVolumeGroupReactivates(t *testing.T) {
	fake := &fakeRunner{
		out: map[string]string{
			joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name", "/dev/nvme0n1"}): "vdo-abc123",
			joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vdo-abc123"}):                               "vdopool abc123",
		},
		err: map[string]error{},
	}
	mgr := lvm.NewManagerWithRunner(fake.run)
	got, err := CreateOrAttach(context.Background(), mgr, "/dev/nvme0n1", "abc123", true, true)
	if err != nil {
		t.Fatalf("CreateOrAttach: %v", err)
	}
	if want := "/dev/vdo-abc123/abc123"; got != want {
		t.Errorf("CreateOrAttach() = %q, want %q", got, want)
	}
	for _, call := range fake.calls {
		if call[0] == "pvcreate" || call[0] == "vgcreate" || call[0] == "lvcreate" {
			t.Errorf("existing volume group must be reactivated, not recreated: got %v", fake.calls)
		}
	}
	wantLast := []string{"vgchange", "-ay", "vdo-abc123"}
	if last := fake.calls[len(fake.calls)-1]; !reflect.DeepEqual(last, wantLast) {
		t.Errorf("last call = %v, want %v", last, wantLast)
	}
}

func TestCreateOrAttach_OrphanedVolumeGroupIsRemovedAndRecreated(t *testing.T) {
	fake := &fakeRunner{
		out: map[string]string{
			joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name", "/dev/nvme0n1"}): "vdo-abc123",
			joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vdo-abc123"}):                               "",
		},
		err: map[string]error{},
	}
	mgr := lvm.NewManagerWithRunner(fake.run)
	if _, err := CreateOrAttach(context.Background(), mgr, "/dev/nvme0n1", "abc123", false, true); err != nil {
		t.Fatalf("CreateOrAttach: %v", err)
	}
	var sawVgremove, sawVgcreate bool
	for _, call := range fake.calls {
		if len(call) >= 1 && call[0] == "vgremove" {
			sawVgremove = true
		}
		if len(call) >= 1 && call[0] == "vgcreate" {
			if !sawVgremove {
				t.Fatal("vgcreate happened before vgremove of the orphaned volume group")
			}
			sawVgcreate = true
		}
	}
	if !sawVgremove || !sawVgcreate {
		t.Errorf("expected both vgremove and vgcreate, got %v", fake.calls)
	}
}

func TestDeactivate_Success(t *testing.T) {
	mgr, fake := newManager()
	if err := Deactivate(context.Background(), mgr, "abc123"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	want := []string{"vgchange", "-an", "vdo-abc123"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestDeactivate_UnreachableDeviceFallsBackToDMCleanup(t *testing.T) {
	fake := &fakeRunner{
		out: map[string]string{
			joinKey([]string{"dmsetup", "ls"}): "No devices found",
		},
		err: map[string]error{
			joinKey([]string{"vgchange", "-an", "vdo-abc123"}): errors.New("Volume group \"vdo-abc123\" not found"),
		},
	}
	mgr := lvm.NewManagerWithRunner(fake.run)
	if err := Deactivate(context.Background(), mgr, "abc123"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	want := [][]string{
		{"vgchange", "-an", "vdo-abc123"},
		{"dmsetup", "ls"},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("recorded calls = %v, want %v", fake.calls, want)
	}
}

func TestDeactivate_OtherErrorIsNotSwallowed(t *testing.T) {
	wantErr := errors.New("device busy")
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{joinKey([]string{"vgchange", "-an", "vdo-abc123"}): wantErr},
	}
	mgr := lvm.NewManagerWithRunner(fake.run)
	err := Deactivate(context.Background(), mgr, "abc123")
	if !errors.Is(err, wantErr) {
		t.Errorf("Deactivate() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestRemove_Success(t *testing.T) {
	mgr, fake := newManager()
	if err := Remove(context.Background(), mgr, "abc123"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	want := [][]string{
		{"vgchange", "-an", "vdo-abc123"},
		{"vgremove", "-f", "vdo-abc123"},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("recorded calls = %v, want %v", fake.calls, want)
	}
}

func TestRemove_UnreachableDeviceFallsBackToDMCleanup(t *testing.T) {
	fake := &fakeRunner{
		out: map[string]string{
			joinKey([]string{"dmsetup", "ls"}): "No devices found",
		},
		err: map[string]error{
			joinKey([]string{"vgremove", "-f", "vdo-abc123"}): errors.New("Volume group \"vdo-abc123\" not found"),
		},
	}
	mgr := lvm.NewManagerWithRunner(fake.run)
	if err := Remove(context.Background(), mgr, "abc123"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	want := [][]string{
		{"vgchange", "-an", "vdo-abc123"},
		{"vgremove", "-f", "vdo-abc123"},
		{"dmsetup", "ls"},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("recorded calls = %v, want %v", fake.calls, want)
	}
}

func TestGrow(t *testing.T) {
	fake := &fakeRunner{
		out: map[string]string{
			joinKey([]string{
				"lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", "vdo-abc123/vdopool",
			}): "8589934592",
		},
		err: map[string]error{},
	}
	mgr := lvm.NewManagerWithRunner(fake.run)
	got, err := Grow(context.Background(), mgr, "/dev/nvme0n1", "abc123")
	if err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if want := "/dev/vdo-abc123/abc123"; got != want {
		t.Errorf("Grow() = %q, want %q", got, want)
	}
	want := [][]string{
		{"pvresize", "/dev/nvme0n1"},
		{"lvextend", "-l+100%FREE", "vdo-abc123/vdopool"},
		{"lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", "vdo-abc123/vdopool"},
		{"lvextend", "-L8589934592B", "vdo-abc123/abc123"},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("recorded calls = %v, want %v", fake.calls, want)
	}
}

func TestResolveClone_ForeignVolumeGroupIsReStamped(t *testing.T) {
	fake := &fakeRunner{
		out: map[string]string{
			joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name", "/dev/nvme0n1"}): "vdo-source",
			joinKey([]string{"lvs", "--noheadings", "-o", "lv_name", "vdo-abc123"}):                               "vdopool source",
		},
		err: map[string]error{},
	}
	mgr := lvm.NewManagerWithRunner(fake.run)
	if err := ResolveClone(context.Background(), mgr, "/dev/nvme0n1", "abc123"); err != nil {
		t.Fatalf("ResolveClone: %v", err)
	}
	var sawImport, sawRename bool
	for _, call := range fake.calls {
		if len(call) >= 1 && call[0] == "vgimportclone" {
			sawImport = true
		}
		if len(call) >= 1 && call[0] == "lvrename" {
			sawRename = true
			want := []string{"lvrename", "vdo-abc123", "source", "abc123"}
			if !reflect.DeepEqual(call, want) {
				t.Errorf("lvrename call = %v, want %v", call, want)
			}
		}
	}
	if !sawImport || !sawRename {
		t.Errorf("expected both vgimportclone and lvrename, got %v", fake.calls)
	}
}

func TestResolveClone_OwnIdentityIsANoOp(t *testing.T) {
	fake := &fakeRunner{
		out: map[string]string{
			joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name", "/dev/nvme0n1"}): "vdo-abc123",
		},
		err: map[string]error{},
	}
	mgr := lvm.NewManagerWithRunner(fake.run)
	if err := ResolveClone(context.Background(), mgr, "/dev/nvme0n1", "abc123"); err != nil {
		t.Fatalf("ResolveClone: %v", err)
	}
	for _, call := range fake.calls {
		if call[0] == "vgimportclone" || call[0] == "lvrename" {
			t.Errorf("device already carrying its own identity must not be re-stamped: got %v", fake.calls)
		}
	}
}

func TestSetFeatures(t *testing.T) {
	mgr, fake := newManager()
	if err := SetFeatures(context.Background(), mgr, "abc123", true, false); err != nil {
		t.Fatalf("SetFeatures: %v", err)
	}
	want := []string{"lvchange", "--compression", "y", "--deduplication", "n", "vdo-abc123/vdopool"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}
