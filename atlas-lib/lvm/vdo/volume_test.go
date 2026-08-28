// Unit tests for the VDO provisioning handler and the VDO feature update:
// they assert the exact lvcreate flags the handler contributes and the
// lvchange command line UpdateVolume builds, since a wrong flag or a wrong
// volume-group path is the failure mode that only surfaces on a live node with
// dm-vdo loaded. Every test drives a real lvm.Manager whose command execution
// is supplied by the fake below, so no lvm2 binary or kernel module is needed.
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
// a method value: that package's runner type is unexported, but a matching func
// value is assignable to it from outside the package.
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

func TestVolumeHandler_CreateVolumeArgs(t *testing.T) {
	tests := []struct {
		name                       string
		compression, deduplication bool
		want                       []string
	}{
		{"both on", true, true, []string{
			"--type", "vdo", "--config", "activation{checks=0}",
			"--compression", "y", "--deduplication", "y",
		}},
		{"compression only", true, false, []string{
			"--type", "vdo", "--config", "activation{checks=0}",
			"--compression", "y", "--deduplication", "n",
		}},
		{"deduplication only", false, true, []string{
			"--type", "vdo", "--config", "activation{checks=0}",
			"--compression", "n", "--deduplication", "y",
		}},
		{"neither, contributes nothing", false, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &volumeHandler{}
			def := lvm.LogicalVolumeDefinition{Compression: tt.compression, Deduplication: tt.deduplication}
			if got := handler.CreateVolumeArgs(def); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CreateVolumeArgs(%+v) = %v, want %v", def, got, tt.want)
			}
		})
	}
}

// Handles agrees with CreateVolumeArgs on which definitions this handler
// contributes flags for: either feature, not both. A mismatch between the two
// would mean CreateLogicalVolume's dispatch (by Handles) and its actual
// argument contribution (by CreateVolumeArgs) disagree about what counts as a
// VDO volume.
func TestVolumeHandler_Handles(t *testing.T) {
	tests := []struct {
		name                       string
		compression, deduplication bool
		want                       bool
	}{
		{"both on", true, true, true},
		{"compression only", true, false, true},
		{"deduplication only", false, true, true},
		{"both off", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &volumeHandler{}
			def := lvm.LogicalVolumeDefinition{Compression: tt.compression, Deduplication: tt.deduplication}
			if got := handler.Handles(def); got != tt.want {
				t.Errorf("Handles(%+v) = %v, want %v", def, got, tt.want)
			}
		})
	}
}

// The handler reaches lvcreate only through the registry this package's init
// populates, so this is what proves the wiring: importing vdo is what makes
// lvm.CreateLogicalVolume produce a VDO volume.
func TestRegisteredHandlerReachesCreateLogicalVolume(t *testing.T) {
	if handler := (&volumeHandler{}); handler.Name() != "vdo" {
		t.Fatalf("Name() = %q, want \"vdo\"", handler.Name())
	}
	mgr, fake := newManager()
	def := lvm.LogicalVolumeDefinition{Compression: true, Deduplication: false}
	if err := mgr.CreateLogicalVolume(context.Background(), "vdo-abc123", "vdopool", "abc123", def); err != nil {
		t.Fatalf("CreateLogicalVolume: %v", err)
	}
	want := []string{
		"lvcreate", "-n", "abc123", "-l", "100%FREE", "vdo-abc123/vdopool", "--yes",
		"--type", "vdo", "--config", "activation{checks=0}",
		"--compression", "y", "--deduplication", "n",
	}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

// UpdateVolume addresses the VDO pool the volume was created on, which is where
// lvm2 keeps the compression and deduplication attributes, and runs unscoped
// like every other volume-group-by-name operation. Exercised through
// NewVolume rather than a struct literal, the same way an external caller
// (csi-driver, every field being unexported) has to build one.
func TestUpdateVolume(t *testing.T) {
	mgr, fake := newManager()
	volume := NewVolume("vdo-abc123", "vdopool", "abc123")
	if err := UpdateVolume(context.Background(), mgr, volume, true, false); err != nil {
		t.Fatalf("UpdateVolume: %v", err)
	}
	want := []string{"lvchange", "--compression", "y", "--deduplication", "n", "vdo-abc123/vdopool"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestUpdateVolume_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("VDO pool is not active")
	key := joinKey([]string{"lvchange", "--compression", "n", "--deduplication", "n", "vdo-abc123/vdopool"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	volume := NewVolume("vdo-abc123", "vdopool", "abc123")
	err := UpdateVolume(context.Background(), lvm.NewManagerWithRunner(fake.run), volume, false, false)
	if !errors.Is(err, wantErr) {
		t.Errorf("UpdateVolume() error = %v, want wrapping %v", err, wantErr)
	}
}
