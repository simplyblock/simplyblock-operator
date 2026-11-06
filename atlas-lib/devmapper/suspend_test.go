// Tests for the split swap the subsystem-flip protocol drives: a suspend that
// holds I/O while the control plane moves the namespace, and a reload-and-resume
// that releases it onto the new device (design-cg-live-subsystem-flip.md §6.3).
package devmapper

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-19: the swap is a path change, not a consistency point, so the suspend
// skips the filesystem freeze but still drains in-flight I/O.
func TestSuspendDrainsInFlightIOWithoutFreezingTheFilesystem(t *testing.T) {
	r := &recorder{}
	if err := New(r.run).Suspend(context.Background(), "sb-v"); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0] != "dmsetup suspend --nolockfs sb-v" {
		t.Fatalf("calls = %v, want one suspend --nolockfs", r.calls)
	}
	if strings.Contains(r.calls[0], "--noflush") {
		t.Fatalf("suspend must drain in-flight I/O, got %q", r.calls[0])
	}
}

func TestReloadResumeLoadsTheNewTableThenResumes(t *testing.T) {
	r := &recorder{}
	if err := New(r.run).ReloadResume(context.Background(), "sb-v", "259:11", 2048); err != nil {
		t.Fatal(err)
	}
	want := "dmsetup reload sb-v --table 0 2048 linear 259:11 0|dmsetup resume sb-v"
	if strings.Join(r.calls, "|") != want {
		t.Fatalf("calls = %v, want %s", r.calls, want)
	}
}

// U-20: unlike the heal path's Swap, a refused reload here must not resume on
// the old table: the namespace may already have left it, and the caller decides
// between retrying and rolling back.
func TestARefusedReloadLeavesTheMappingSuspended(t *testing.T) {
	r := &recorder{err: map[string]error{"dmsetup reload": errors.New("invalid table")}}
	if err := New(r.run).ReloadResume(context.Background(), "sb-v", "259:11", 2048); err == nil {
		t.Fatal("refused reload reported success")
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "dmsetup resume") {
			t.Fatalf("resumed after a refused reload: %v", r.calls)
		}
	}
}

func TestResumeResumesTheMapping(t *testing.T) {
	r := &recorder{}
	if err := New(r.run).Resume(context.Background(), "sb-v"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.calls, "|") != "dmsetup resume sb-v" {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestSuspendedReadsTheMappingState(t *testing.T) {
	for out, want := range map[string]bool{"sb-v:Suspended\n": true, "sb-v:Active\n": false} {
		r := &recorder{out: map[string]string{"dmsetup info": out}}
		got, err := New(r.run).Suspended(context.Background(), "sb-v")
		if err != nil || got != want {
			t.Errorf("%q: Suspended = %v, %v; want %v", out, got, err, want)
		}
	}
}

func TestSuspendedOfAMissingMappingIsErrNotFound(t *testing.T) {
	r := &recorder{
		out: map[string]string{"dmsetup info": "Device sb-v not found"},
		err: map[string]error{"dmsetup info": errors.New("exit status 1")},
	}
	if _, err := New(r.run).Suspended(context.Background(), "sb-v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListReturnsOnlyMappingsWithThePrefix(t *testing.T) {
	r := &recorder{out: map[string]string{"dmsetup info": "sb-a:Suspended\nvdo--x-vpool:Active\nsb-b:Active\n"}}
	got, err := New(r.run).List(context.Background(), "sb-")
	if err != nil {
		t.Fatal(err)
	}
	want := []Mapping{{Name: "sb-a", Suspended: true}, {Name: "sb-b"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
}

func TestListWithNoMappingsIsEmpty(t *testing.T) {
	r := &recorder{out: map[string]string{"dmsetup info": "No devices found\n"}}
	got, err := New(r.run).List(context.Background(), "sb-")
	if err != nil || len(got) != 0 {
		t.Fatalf("List = %+v, %v", got, err)
	}
}
