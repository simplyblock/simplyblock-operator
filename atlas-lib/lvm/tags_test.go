// What the tag primitives issue, and how a listing is read back.

package lvm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestVolumeGroupTagsRoundTrip(t *testing.T) {
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{}}
	mgr := NewManagerWithRunner(fake.run)
	vg := VolumeGroup{Name: "vol-abc123"}

	if err := mgr.AddVolumeGroupTag(context.Background(), vg, "simplyblock.creating"); err != nil {
		t.Fatalf("AddVolumeGroupTag: %v", err)
	}
	if err := mgr.RemoveVolumeGroupTag(context.Background(), vg, "simplyblock.creating"); err != nil {
		t.Fatalf("RemoveVolumeGroupTag: %v", err)
	}
	want := [][]string{
		{"vgchange", "--addtag", "simplyblock.creating", "vol-abc123"},
		{"vgchange", "--deltag", "simplyblock.creating", "vol-abc123"},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("issued %v, want %v", fake.calls, want)
	}
}

// vgs prints a group's tags on one line, comma-separated and indented, and a
// group with none prints an empty line. Both are readings, not errors.
func TestVolumeGroupTagsReadsTheListing(t *testing.T) {
	vg := VolumeGroup{Name: "vol-abc123"}
	key := joinKey([]string{"vgs", "--noheadings", "-o", "vg_tags", "vol-abc123"})
	for name, tt := range map[string]struct {
		out  string
		want []string
	}{
		"two tags":     {"  simplyblock.creating,other\n", []string{"simplyblock.creating", "other"}},
		"one tag":      {"  simplyblock.creating\n", []string{"simplyblock.creating"}},
		"no tags":      {"  \n", nil},
		"warning line": {"  WARNING: something\n  simplyblock.creating\n", []string{"simplyblock.creating"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRunner{out: map[string]string{key: tt.out}, err: map[string]error{}}
			got, err := NewManagerWithRunner(fake.run).VolumeGroupTags(context.Background(), vg)
			if err != nil {
				t.Fatalf("VolumeGroupTags: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A vgs that failed is not a group with no tags.
func TestVolumeGroupTagsReturnsAFailedListing(t *testing.T) {
	vg := VolumeGroup{Name: "vol-abc123"}
	key := joinKey([]string{"vgs", "--noheadings", "-o", "vg_tags", "vol-abc123"})
	fake := &fakeRunner{out: map[string]string{}, err: map[string]error{key: errors.New("boom")}}
	if _, err := NewManagerWithRunner(fake.run).VolumeGroupTags(context.Background(), vg); err == nil {
		t.Fatal("a failed vgs was read as a group without tags")
	}
}
