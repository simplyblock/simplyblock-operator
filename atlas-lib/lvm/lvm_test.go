package lvm

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fakeRunner records every command line it's asked to run and answers from a
// script keyed by the joined args, so no lvm2 binary has to be present.
// Shared by every test file in this package.
type fakeRunner struct {
	calls [][]string
	out   map[string]string
	err   map[string]error

	// unowned makes every group the fake reports carry no OwnerTag, which is
	// the shape a test of a refusal sets. Left false, a tags listing nobody
	// scripted answers with the tag, since every group the driver makes has it.
	unowned bool
}

func (f *fakeRunner) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	key := joinKey(args)
	// An adoption is what makes a group read as owned from then on: unscoped,
	// every later listing answers with the tag; on a device, that device does.
	if args[0] == "vgchange" && slices.Contains(args, "--addtag") && slices.Contains(args, OwnerTag) {
		if args[1] == "--devices" {
			dev, group := args[2], args[len(args)-1]
			f.out[joinKey([]string{"pvs", "--devices", dev, "--noheadings", "-o", "vg_name,vg_tags", dev})] = "  " + group + " " + OwnerTag + "\n"
		} else {
			f.unowned = false
		}
	}
	if out, ok := f.out[key]; ok || f.err[key] != nil {
		return out, f.err[key]
	}
	if isTagsListing(args) && !f.unowned {
		if args[0] == "pvs" {
			// vg_name,vg_tags for the device: a group named after the operand.
			return "  vg1 " + OwnerTag + "\n", nil
		}
		return "  " + OwnerTag + "\n", nil
	}
	return "", nil
}

// isTagsListing reports whether args read a group's tags.
func isTagsListing(args []string) bool {
	if len(args) == 0 || (args[0] != "vgs" && args[0] != "pvs") {
		return false
	}
	for _, a := range args {
		if strings.Contains(a, "vg_tags") {
			return true
		}
	}
	return false
}

// mutating is the calls that change something: everything but the listings the
// ownership check and the identity probes read.
func (f *fakeRunner) mutating() [][]string {
	var out [][]string
	for _, call := range f.calls {
		switch call[0] {
		case "vgs", "pvs", "lvs":
			continue
		}
		out = append(out, call)
	}
	return out
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
			if got := deviceScope(tt.devices...); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("deviceScope(%v) = %v, want %v", tt.devices, got, tt.want)
			}
		})
	}
}

func TestManager_exec_InsertsDeviceScopeAfterBinary(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	_, err := mgr.exec(context.Background(), []string{"/dev/nvme0n1"}, "pvcreate", "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := []string{"pvcreate", "--devices", "/dev/nvme0n1", "/dev/nvme0n1"}
	if !reflect.DeepEqual(fake.mutating(), [][]string{want}) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_exec_NoDevicesRunsUnscoped(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	_, err := mgr.exec(context.Background(), nil, "vgchange", "-an", "vdo-abc123")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := []string{"vgchange", "-an", "vdo-abc123"}
	if !reflect.DeepEqual(fake.mutating(), [][]string{want}) {
		t.Errorf("recorded call = %v, want %v (no --devices flag)", fake.calls, want)
	}
}

// Run is the escape hatch and takes no device list at all: a command that has
// to be scoped gets a named method instead.
func TestManager_Run_IsUnscoped(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	if _, err := mgr.Run(context.Background(), "lvchange", "--compression", "y", "vdo-abc123/vdopool"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"lvchange", "--compression", "y", "vdo-abc123/vdopool"}
	if !reflect.DeepEqual(fake.mutating(), [][]string{want}) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_Run_RequiresACommandName(t *testing.T) {
	mgr := NewManagerWithRunner(nil)
	if _, err := mgr.Run(context.Background()); err == nil {
		t.Error("expected an error for a call with no command name")
	}
}
