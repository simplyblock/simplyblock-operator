package lvm

import (
	"context"
	"reflect"
	"testing"
)

// The registry existed with nothing registered against it: neither this
// package nor its former lvm/vdo subpackage ever called
// RegisterVolumeProvisioning for a real "vdo" handler, so CreateLogicalVolume
// silently created a plain linear volume for a LogicalVolumeDefinition asking
// for compression or deduplication. This is the handler that closes that gap,
// registered by this package's own init so a caller cannot forget the import
// that used to be required to reach it.
func TestVDOProvisioning_Handles(t *testing.T) {
	cases := []struct {
		name        string
		def         LogicalVolumeDefinition
		wantHandled bool
	}{
		{"neither", LogicalVolumeDefinition{}, false},
		{"compression only", LogicalVolumeDefinition{Compression: true}, true},
		{"deduplication only", LogicalVolumeDefinition{Deduplication: true}, true},
		{"both", LogicalVolumeDefinition{Compression: true, Deduplication: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (&vdoProvisioning{}).Handles(c.def); got != c.wantHandled {
				t.Errorf("Handles(%+v) = %v, want %v", c.def, got, c.wantHandled)
			}
		})
	}
}

func TestVDOProvisioning_CreateVolumeArgs(t *testing.T) {
	cases := []struct {
		name string
		def  LogicalVolumeDefinition
		want []string
	}{
		{
			"compression and deduplication",
			LogicalVolumeDefinition{Compression: true, Deduplication: true},
			[]string{"--type", "vdo", "--config", "activation{checks=0}", "--compression", "y", "--deduplication", "y"},
		},
		{
			"compression only",
			LogicalVolumeDefinition{Compression: true},
			[]string{"--type", "vdo", "--config", "activation{checks=0}", "--compression", "y", "--deduplication", "n"},
		},
		{
			"deduplication only",
			LogicalVolumeDefinition{Deduplication: true},
			[]string{"--type", "vdo", "--config", "activation{checks=0}", "--compression", "n", "--deduplication", "y"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := (&vdoProvisioning{}).CreateVolumeArgs(c.def)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("CreateVolumeArgs(%+v) = %v, want %v", c.def, got, c.want)
			}
		})
	}
}

// This is the same gap at the level a real caller hits it: CreateLogicalVolume
// with a VDO-shaped definition must actually produce a VDO lvcreate, via
// whatever handler is registered at package init, without the caller doing
// anything to opt in.
func TestManager_CreateLogicalVolume_VDODefinitionProducesVDOCommand(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	def := LogicalVolumeDefinition{Compression: true, Deduplication: true}
	vg := VolumeGroup{Name: "vdo-lvol1"}
	if _, err := mgr.CreateLogicalVolume(context.Background(), vg, "vdopool", "lvol1", def); err != nil {
		t.Fatalf("CreateLogicalVolume: %v", err)
	}
	want := []string{
		"lvcreate", "-n", "lvol1", "-l", "100%FREE", "vdo-lvol1/vdopool", "--yes",
		"--type", "vdo", "--config", "activation{checks=0}", "--compression", "y", "--deduplication", "y",
	}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", fake.calls, want)
	}
}
