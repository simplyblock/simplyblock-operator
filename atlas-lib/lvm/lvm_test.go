package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeRunner records every command line it's asked to run and answers from a
// script keyed by the joined args, so no lvm2 binary has to be present.
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

func TestInspector_Run_InsertsDeviceScopeAfterBinary(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	insp := NewInspectorWithRunner(fake.run)

	_, err := insp.Run(context.Background(), []string{"/dev/nvme0n1"}, "pvcreate", "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"pvcreate", "--devices", "/dev/nvme0n1", "/dev/nvme0n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestInspector_Run_NoDevicesRunsUnscoped(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	insp := NewInspectorWithRunner(fake.run)

	_, err := insp.Run(context.Background(), nil, "vgchange", "-an", "vdo-abc123")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"vgchange", "-an", "vdo-abc123"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v (no --devices flag)", fake.calls, want)
	}
}

func TestInspector_Run_RequiresACommandName(t *testing.T) {
	insp := NewInspectorWithRunner(nil)
	if _, err := insp.Run(context.Background(), []string{"/dev/nvme0n1"}); err == nil {
		t.Error("expected an error for a call with no command name")
	}
}

func TestInspector_VolumeGroup(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"blank device", "", nil, ""},
		{"belongs to a VG", "  vdo-abc123  \n", nil, "vdo-abc123"},
		{
			"WARNING lines ahead of the real field, from a duplicate-PV clone",
			"WARNING: PV a is duplicate for PVID b\n  vdo-abc123\n",
			nil,
			"vdo-abc123",
		},
		{"probe itself fails", "", errors.New("device or resource busy"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := joinKey([]string{"pvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "vg_name", "/dev/nvme0n1"})
			fake := &fakeRunner{
				out: map[string]string{key: tt.out},
				err: map[string]error{key: tt.err},
			}
			insp := NewInspectorWithRunner(fake.run)
			got, err := insp.VolumeGroup(context.Background(), "/dev/nvme0n1")
			if err != nil {
				t.Fatalf("VolumeGroup: %v", err)
			}
			if got != tt.want {
				t.Errorf("VolumeGroup() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInspector_HasLogicalVolume(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"lv present among others", "  poolvol\n  data1\n", nil, true},
		{"orphaned VG, zero LVs", "", nil, false},
		{"lv absent", "  poolvol\n", nil, false},
		{"unreadable VG", "", errors.New("failed to find VG"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := joinKey([]string{"lvs", "--devices", "/dev/nvme0n1", "--noheadings", "-o", "lv_name", "vg1"})
			fake := &fakeRunner{
				out: map[string]string{key: tt.out},
				err: map[string]error{key: tt.err},
			}
			insp := NewInspectorWithRunner(fake.run)
			got, err := insp.HasLogicalVolume(context.Background(), []string{"/dev/nvme0n1"}, "vg1", "data1")
			if err != nil {
				t.Fatalf("HasLogicalVolume: %v", err)
			}
			if got != tt.want {
				t.Errorf("HasLogicalVolume() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInspector_Rescan(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	insp := NewInspectorWithRunner(fake.run)

	if err := insp.Rescan(context.Background(), []string{"/dev/nvme0n1", "/dev/nvme1n1"}); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	want := []string{"pvscan", "--devices", "/dev/nvme0n1,/dev/nvme1n1", "--cache"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestInspector_Rescan_PropagatesRunnerError(t *testing.T) {
	wantErr := errors.New("lock contention")
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{joinKey([]string{"pvscan", "--devices", "/dev/nvme0n1", "--cache"}): wantErr},
	}
	insp := NewInspectorWithRunner(fake.run)

	if err := insp.Rescan(context.Background(), []string{"/dev/nvme0n1"}); !errors.Is(err, wantErr) {
		t.Errorf("Rescan() error = %v, want %v", err, wantErr)
	}
}

func TestEscapeDMName(t *testing.T) {
	tests := []struct{ name, want string }{
		{"vdo-abc123", "vdo--abc123"},
		{"plain", "plain"},
		{"a-b-c", "a--b--c"},
	}
	for _, tt := range tests {
		if got := EscapeDMName(tt.name); got != tt.want {
			t.Errorf("EscapeDMName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestInspector_RemoveOrphanedDMNodes(t *testing.T) {
	t.Run("no matching nodes", func(t *testing.T) {
		fake := &fakeRunner{
			out: map[string]string{joinKey([]string{"dmsetup", "ls"}): "No devices found"},
			err: map[string]error{},
		}
		insp := NewInspectorWithRunner(fake.run)
		if err := insp.RemoveOrphanedDMNodes(context.Background(), "vdo-abc123"); err != nil {
			t.Fatalf("RemoveOrphanedDMNodes: %v", err)
		}
	})

	t.Run("matches escaped names and removes them", func(t *testing.T) {
		lsOut := "vdo--abc123-vdopool-vpool\t(253:3)\n" +
			"vdo--abc123-vdopool_vdata\t(253:2)\n" +
			"rl-root\t(253:0)\n"
		fake := &fakeRunner{
			out: map[string]string{joinKey([]string{"dmsetup", "ls"}): lsOut},
			err: map[string]error{},
		}
		insp := NewInspectorWithRunner(fake.run)
		if err := insp.RemoveOrphanedDMNodes(context.Background(), "vdo-abc123"); err != nil {
			t.Fatalf("RemoveOrphanedDMNodes: %v", err)
		}

		removed := map[string]bool{}
		for _, call := range fake.calls {
			if len(call) == 3 && call[0] == "dmsetup" && call[1] == "remove" {
				removed[call[2]] = true
			}
		}
		if !removed["vdo--abc123-vdopool-vpool"] || !removed["vdo--abc123-vdopool_vdata"] {
			t.Errorf("expected both orphaned nodes removed, got calls %v", fake.calls)
		}
		if removed["rl-root"] {
			t.Error("unrelated dm node rl-root must not be removed")
		}
	})

	t.Run("a node stuck on pass one clears once its dependent is gone", func(t *testing.T) {
		removePool := joinKey([]string{"dmsetup", "remove", "vdo--abc123-vdopool-vpool"})
		poolAttempts := 0
		run := func(_ context.Context, args ...string) (string, error) {
			switch joinKey(args) {
			case joinKey([]string{"dmsetup", "ls"}):
				return "vdo--abc123-vdopool-vpool\t(253:3)\nvdo--abc123-vdopool_vdata\t(253:2)\n", nil
			case removePool:
				poolAttempts++
				if poolAttempts == 1 {
					// The pool depends on vdata: removing it first fails, then succeeds
					// on a later pass once vdata is already gone.
					return "", errors.New("device is in use")
				}
				return "", nil
			default:
				return "", nil
			}
		}
		insp := NewInspectorWithRunner(run)
		if err := insp.RemoveOrphanedDMNodes(context.Background(), "vdo-abc123"); err != nil {
			t.Fatalf("RemoveOrphanedDMNodes: %v", err)
		}
		if poolAttempts < 2 {
			t.Errorf("expected the pool node to be retried on a later pass, got %d attempt(s)", poolAttempts)
		}
	})

	t.Run("dmsetup ls itself fails", func(t *testing.T) {
		wantErr := errors.New("dmsetup: command not found")
		fake := &fakeRunner{
			out: map[string]string{},
			err: map[string]error{joinKey([]string{"dmsetup", "ls"}): wantErr},
		}
		insp := NewInspectorWithRunner(fake.run)
		if err := insp.RemoveOrphanedDMNodes(context.Background(), "vdo-abc123"); !errors.Is(err, wantErr) {
			t.Errorf("RemoveOrphanedDMNodes() error = %v, want wrapping %v", err, wantErr)
		}
	})
}
