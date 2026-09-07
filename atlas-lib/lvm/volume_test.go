package lvm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestManager_CreatePhysicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	pv, err := mgr.CreatePhysicalVolume(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"})
	if err != nil {
		t.Fatalf("CreatePhysicalVolume: %v", err)
	}
	if pv.DevicePath != "/dev/nvme0n1" {
		t.Errorf("CreatePhysicalVolume() = %v, want DevicePath /dev/nvme0n1", pv)
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
	if _, err := mgr.CreatePhysicalVolume(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"}); !errors.Is(err, wantErr) {
		t.Errorf("CreatePhysicalVolume() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_CreateVolumeGroup(t *testing.T) {
	t.Run("single device", func(t *testing.T) {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)
		vg, err := mgr.CreateVolumeGroup(context.Background(), VolumeGroup{Name: "vg1"}, PhysicalVolume{DevicePath: "/dev/nvme0n1"})
		if err != nil {
			t.Fatalf("CreateVolumeGroup: %v", err)
		}
		if vg.Name != "vg1" {
			t.Errorf("CreateVolumeGroup() = %v, want Name vg1", vg)
		}
		want := []string{"vgcreate", "--devices", "/dev/nvme0n1", "vg1", "/dev/nvme0n1"}
		if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
			t.Errorf("recorded call = %v, want %v", fake.calls, want)
		}
	})

	t.Run("multiple devices, for a striped VG", func(t *testing.T) {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)
		pvs := []PhysicalVolume{{DevicePath: "/dev/nvme0n1"}, {DevicePath: "/dev/nvme1n1"}}
		if _, err := mgr.CreateVolumeGroup(context.Background(), VolumeGroup{Name: "vg1"}, pvs...); err != nil {
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
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.ActivateVolumeGroup(context.Background(), VolumeGroup{Name: "vg1"}); err != nil {
		t.Fatalf("ActivateVolumeGroup: %v", err)
	}
	want := []string{"vgchange", "-ay", "vg1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_DeactivateVolumeGroup(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.DeactivateVolumeGroup(context.Background(), VolumeGroup{Name: "vg1"}); err != nil {
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
	if err := mgr.DeactivateVolumeGroup(context.Background(), VolumeGroup{Name: "vg1"}); !errors.Is(err, wantErr) {
		t.Errorf("DeactivateVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_RemoveVolumeGroup(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	if err := mgr.RemoveVolumeGroup(context.Background(), VolumeGroup{Name: "vg1"}); err != nil {
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
	if err := mgr.RemoveVolumeGroup(context.Background(), VolumeGroup{Name: "vg1"}); !errors.Is(err, wantErr) {
		t.Errorf("RemoveVolumeGroup() error = %v, want wrapping %v", err, wantErr)
	}
}

// fakeVolumeProvisioning is registered under a name other than "vdo" by the
// test below, so a pass proves CreateLogicalVolume dispatches by asking each
// registered handler whether it Handles the definition, not by hardcoding a
// lookup of the "vdo" key.
type fakeVolumeProvisioning struct {
	name    string
	handles func(LogicalVolumeDefinition) bool
	args    []string
}

func (f *fakeVolumeProvisioning) Name() string { return f.name }

func (f *fakeVolumeProvisioning) Handles(def LogicalVolumeDefinition) bool { return f.handles(def) }

func (f *fakeVolumeProvisioning) CreateVolumeArgs(LogicalVolumeDefinition) []string { return f.args }

func TestManager_CreateLogicalVolume_DispatchesByHandles(t *testing.T) {
	RegisterVolumeProvisioning(&fakeVolumeProvisioning{
		name:    "fake-provisioner",
		handles: func(def LogicalVolumeDefinition) bool { return def.Compression },
		args:    []string{"--fake-flag"},
	})

	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	def := LogicalVolumeDefinition{Compression: true}
	vg := VolumeGroup{Name: "vg1"}
	lv, err := mgr.CreateLogicalVolume(context.Background(), vg, "pv1", "lv1", def)
	if err != nil {
		t.Fatalf("CreateLogicalVolume: %v", err)
	}
	if want := (LogicalVolume{VolumeGroup: vg, Name: "lv1"}); lv != want {
		t.Errorf("CreateLogicalVolume() = %v, want %v", lv, want)
	}
	want := []string{"lvcreate", "-n", "lv1", "-l", "100%FREE", "vg1/pv1", "--yes", "--fake-flag"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_CreateLogicalVolume_NoHandlerMatchesContributesNothing(t *testing.T) {
	RegisterVolumeProvisioning(&fakeVolumeProvisioning{
		name:    "fake-provisioner-2",
		handles: func(LogicalVolumeDefinition) bool { return false },
		args:    []string{"--should-never-appear"},
	})

	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	vg := VolumeGroup{Name: "vg1"}
	if _, err := mgr.CreateLogicalVolume(context.Background(), vg, "pv1", "lv1", LogicalVolumeDefinition{}); err != nil {
		t.Fatalf("CreateLogicalVolume: %v", err)
	}
	want := []string{"lvcreate", "-n", "lv1", "-l", "100%FREE", "vg1/pv1", "--yes"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

func TestManager_RemovePhysicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	if err := mgr.RemovePhysicalVolume(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"}); err != nil {
		t.Fatalf("RemovePhysicalVolume: %v", err)
	}
	want := []string{"pvremove", "--devices", "/dev/nvme0n1", "--yes", "/dev/nvme0n1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

// Regression: 2026-09-05-pvremove-forced-over-a-live-volume-group. The removal
// was first written with pvremove --force --force, caught in review before it
// shipped. Those are the flags LVM asks for in order to wipe a label the caller
// has already been told is still claimed by a volume group.
//
// Passing it unconditionally trades the one check LVM performs here for an
// assumption about the caller, and the assumption fails exactly where it costs
// most: a teardown that crashed between the vgremove and the pvremove, a plan
// naming the wrong device, or a clone staged beside its source. The refusal is
// the feature, so the flag is never passed and a caller that hits the refusal
// hears about it.
func TestManager_RemovePhysicalVolume_NeverForces(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	if err := mgr.RemovePhysicalVolume(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"}); err != nil {
		t.Fatalf("RemovePhysicalVolume: %v", err)
	}
	for _, arg := range fake.calls[0] {
		switch arg {
		case "--force", "-f", "-ff":
			t.Errorf("pvremove was passed %q, which overrides LVM's refusal to wipe a label still in use: %v",
				arg, fake.calls[0])
		}
	}
}

// A device that carries no physical-volume label is already in the state the
// caller asked for, so removing one is convergent rather than an error: a
// deletion may resume after a crash that happened between the pvremove and
// whatever recorded it.
func TestManager_RemovePhysicalVolume_AbsentLabelIsNotAnError(t *testing.T) {
	key := joinKey([]string{"pvremove", "--devices", "/dev/nvme0n1", "--yes", "/dev/nvme0n1"})
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{key: errors.New("No PV label found on /dev/nvme0n1")},
	}
	mgr := NewManagerWithRunner(fake.run)

	if err := mgr.RemovePhysicalVolume(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"}); err != nil {
		t.Errorf("RemovePhysicalVolume on a device with no label = %v, want nil", err)
	}
}

// A label LVM refuses to wipe because a volume group still claims it surfaces as
// an error. It is the one refusal that stands between a mis-ordered teardown and
// a live volume group losing the device underneath it.
func TestManager_RemovePhysicalVolume_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("device is used by a volume group")
	key := joinKey([]string{"pvremove", "--devices", "/dev/nvme0n1", "--yes", "/dev/nvme0n1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)

	if err := mgr.RemovePhysicalVolume(context.Background(), PhysicalVolume{DevicePath: "/dev/nvme0n1"}); !errors.Is(err, wantErr) {
		t.Errorf("RemovePhysicalVolume() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestManager_RemoveLogicalVolume(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	lv := LogicalVolume{VolumeGroup: VolumeGroup{Name: "vg1"}, Name: "lv1"}
	if err := mgr.RemoveLogicalVolume(context.Background(), lv); err != nil {
		t.Fatalf("RemoveLogicalVolume: %v", err)
	}
	want := []string{"lvremove", "--yes", "vg1/lv1"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

// A logical volume that is already gone is the state the caller asked for, which
// a deletion resuming after a crash depends on.
func TestManager_RemoveLogicalVolume_AbsentIsNotAnError(t *testing.T) {
	key := joinKey([]string{"lvremove", "--yes", "vg1/lv1"})
	fake := &fakeRunner{
		out: map[string]string{},
		err: map[string]error{key: errors.New(`Failed to find logical volume "vg1/lv1"`)},
	}
	mgr := NewManagerWithRunner(fake.run)

	lv := LogicalVolume{VolumeGroup: VolumeGroup{Name: "vg1"}, Name: "lv1"}
	if err := mgr.RemoveLogicalVolume(context.Background(), lv); err != nil {
		t.Errorf("RemoveLogicalVolume on a volume that is gone = %v, want nil", err)
	}
}

// A logical volume something still has open is a removal LVM refuses, and the
// refusal is returned rather than retried past.
func TestManager_RemoveLogicalVolume_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("Logical volume vg1/lv1 contains a filesystem in use")
	key := joinKey([]string{"lvremove", "--yes", "vg1/lv1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)

	lv := LogicalVolume{VolumeGroup: VolumeGroup{Name: "vg1"}, Name: "lv1"}
	if err := mgr.RemoveLogicalVolume(context.Background(), lv); !errors.Is(err, wantErr) {
		t.Errorf("RemoveLogicalVolume() error = %v, want wrapping %v", err, wantErr)
	}
}

// LogicalVolumeActive reads the state character of lv_attr, which is what tells a
// complete volume group apart from one whose logical volume is merely not mapped
// on this host. The distinction decides between reactivating and creating.
func TestManager_LogicalVolumeActive(t *testing.T) {
	cases := []struct {
		name string
		attr string
		want bool
	}{
		{"a linear volume, active", "  -wi-a-----\n", true},
		{"a linear volume, not active", "  -wi-------\n", false},
		{"a VDO volume, active", "  vwi-a-v---\n", true},
		{"a VDO volume, not active", "  vwi---v---\n", false},
		{"a thin pool, active and open", "  twi-aotz--\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := joinKey([]string{"lvs", "--noheadings", "-o", "lv_attr", "vg1/lv1"})
			fake := &fakeRunner{out: map[string]string{key: tc.attr}, err: map[string]error{}}
			mgr := NewManagerWithRunner(fake.run)

			lv := LogicalVolume{VolumeGroup: VolumeGroup{Name: "vg1"}, Name: "lv1"}
			active, err := mgr.LogicalVolumeActive(context.Background(), lv)
			if err != nil {
				t.Fatalf("LogicalVolumeActive: %v", err)
			}
			if active != tc.want {
				t.Errorf("LogicalVolumeActive(%q) = %v, want %v", strings.TrimSpace(tc.attr), active, tc.want)
			}
		})
	}
}

// An attribute string too short to carry a state character is not a reading of
// not active. Folding it into false would have a caller activate a volume group
// whose real state it never learned.
func TestManager_LogicalVolumeActive_UnreadableAttributes(t *testing.T) {
	for _, attr := range []string{"", "\n", "  -wi\n"} {
		key := joinKey([]string{"lvs", "--noheadings", "-o", "lv_attr", "vg1/lv1"})
		fake := &fakeRunner{out: map[string]string{key: attr}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)

		lv := LogicalVolume{VolumeGroup: VolumeGroup{Name: "vg1"}, Name: "lv1"}
		if _, err := mgr.LogicalVolumeActive(context.Background(), lv); err == nil {
			t.Errorf("LogicalVolumeActive(%q) folded an unreadable answer into a state", attr)
		}
	}
}

func TestManager_LogicalVolumeActive_WrapsRunnerError(t *testing.T) {
	wantErr := errors.New("volume group not found")
	key := joinKey([]string{"lvs", "--noheadings", "-o", "lv_attr", "vg1/lv1"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: wantErr}}
	mgr := NewManagerWithRunner(fake.run)

	lv := LogicalVolume{VolumeGroup: VolumeGroup{Name: "vg1"}, Name: "lv1"}
	if _, err := mgr.LogicalVolumeActive(context.Background(), lv); !errors.Is(err, wantErr) {
		t.Errorf("LogicalVolumeActive() error = %v, want wrapping %v", err, wantErr)
	}
}

// A logical volume with no pool targets the volume group itself. lvcreate's
// <vg>/<pool> form names the pool a new volume is created inside, which only a
// pool-based type needs; a linear or striped volume has none, and "vg1/" is not a
// target lvcreate accepts.
func TestManager_CreateLogicalVolume_NoPoolTargetsTheVolumeGroup(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	vg := VolumeGroup{Name: "vg1"}
	if _, err := mgr.CreateLogicalVolume(context.Background(), vg, "", "lv1", LogicalVolumeDefinition{}); err != nil {
		t.Fatalf("CreateLogicalVolume: %v", err)
	}
	want := []string{"lvcreate", "-n", "lv1", "-l", "100%FREE", "vg1", "--yes"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

// A striped volume says so at creation time. A volume group spanning several
// members produces a linear volume unless lvcreate is told otherwise, and a
// filesystem above one aligned to a stripe that was never laid down is aligned to
// nothing.
func TestManager_CreateLogicalVolume_Striped(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	vg := VolumeGroup{Name: "vg1"}
	def := LogicalVolumeDefinition{Stripes: 4, StripeChunkBytes: 65536}
	if _, err := mgr.CreateLogicalVolume(context.Background(), vg, "", "lv1", def); err != nil {
		t.Fatalf("CreateLogicalVolume: %v", err)
	}
	want := []string{"lvcreate", "-n", "lv1", "-l", "100%FREE", "-i", "4", "-I", "64k", "vg1", "--yes"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}

// One stripe is not a stripe, so nothing is passed for it: lvcreate -i 1 is a
// linear volume spelled the long way, and a chunk size with no stripe count has
// nothing to apply to.
func TestManager_CreateLogicalVolume_DegenerateStripesAreOmitted(t *testing.T) {
	for _, def := range []LogicalVolumeDefinition{
		{Stripes: 1, StripeChunkBytes: 65536},
		{Stripes: 0, StripeChunkBytes: 65536},
	} {
		fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
		mgr := NewManagerWithRunner(fake.run)

		vg := VolumeGroup{Name: "vg1"}
		if _, err := mgr.CreateLogicalVolume(context.Background(), vg, "", "lv1", def); err != nil {
			t.Fatalf("CreateLogicalVolume: %v", err)
		}
		for _, arg := range fake.calls[0] {
			if arg == "-i" || arg == "-I" {
				t.Errorf("CreateLogicalVolume(%+v) passed %q: %v", def, arg, fake.calls[0])
			}
		}
	}
}

// A stripe count with no chunk size lets LVM pick the chunk, which is a valid
// striped volume and the caller's choice to make.
func TestManager_CreateLogicalVolume_StripesWithoutAChunkSize(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)

	vg := VolumeGroup{Name: "vg1"}
	if _, err := mgr.CreateLogicalVolume(
		context.Background(), vg, "", "lv1", LogicalVolumeDefinition{Stripes: 4}); err != nil {
		t.Fatalf("CreateLogicalVolume: %v", err)
	}
	want := []string{"lvcreate", "-n", "lv1", "-l", "100%FREE", "-i", "4", "vg1", "--yes"}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}
