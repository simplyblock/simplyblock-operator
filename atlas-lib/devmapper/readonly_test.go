// Tests for read-only mappings: creating one, reading the mode back, making an
// existing mapping read-only, and keeping it read-only across every reload.

package devmapper

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

const attrQuery = "dmsetup info -c --noheadings -o attr sb-v"

func TestCreateReadOnlyLoadsAReadOnlyTable(t *testing.T) {
	r := &recorder{}
	if err := New(r.run).CreateReadOnly(context.Background(), "sb-v", "259:3", 2048); err != nil {
		t.Fatal(err)
	}
	want := []string{"dmsetup create --readonly sb-v --table 0 2048 linear 259:3 0"}
	if !slices.Equal(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
}

func TestReadOnlyReadsTheMappingsAttributes(t *testing.T) {
	for attr, want := range map[string]bool{"L--r\n": true, "L--w\n": false, "Ls-r": true} {
		r := &recorder{out: map[string]string{attrQuery: attr}}
		got, err := New(r.run).ReadOnly(context.Background(), "sb-v")
		if err != nil || got != want {
			t.Errorf("ReadOnly with attributes %q = %v, %v; want %v", attr, got, err, want)
		}
	}
}

func TestReadOnlyOfAMissingMappingIsErrNotFound(t *testing.T) {
	r := &recorder{
		out: map[string]string{"dmsetup info": "Device sb-v not found"},
		err: map[string]error{"dmsetup info": errors.New("exit status 1")},
	}
	if _, err := New(r.run).ReadOnly(context.Background(), "sb-v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Regression: 2026-11-25-dm-reload-drops-readonly. A plain dmsetup reload loads
// a writable table, verified on a RHEL 9 host: a mapping created --readonly
// reads back L--w after one. Swap is what a namespace move and an expand run,
// so a read-only volume would quietly start taking writes after either.
func TestSwapKeepsAReadOnlyMappingReadOnly(t *testing.T) {
	r := &recorder{out: map[string]string{attrQuery: "L--r\n"}}
	if err := New(r.run).Swap(context.Background(), "sb-v", "259:11", 2048); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.calls, "dmsetup reload --readonly sb-v --table 0 2048 linear 259:11 0") {
		t.Fatalf("calls = %q, want a read-only reload", r.calls)
	}
}

// The same for the swap service's reload of a mapping it suspended.
func TestReloadResumeKeepsAReadOnlyMappingReadOnly(t *testing.T) {
	r := &recorder{out: map[string]string{attrQuery: "Ls-r\n"}}
	if err := New(r.run).ReloadResume(context.Background(), "sb-v", "259:11", 2048); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.calls, "dmsetup reload --readonly sb-v --table 0 2048 linear 259:11 0") {
		t.Fatalf("calls = %q, want a read-only reload", r.calls)
	}
}

func TestSwapKeepsAWritableMappingWritable(t *testing.T) {
	r := &recorder{out: map[string]string{attrQuery: "L--w\n"}}
	if err := New(r.run).Swap(context.Background(), "sb-v", "259:11", 2048); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "--readonly") {
			t.Fatalf("calls = %q, want no read-only load", r.calls)
		}
	}
}

// A mapping staged writable by an earlier driver is converted in place, on the
// table it already has.
func TestMakeReadOnlyReloadsTheSameTableReadOnly(t *testing.T) {
	r := &recorder{out: map[string]string{"dmsetup table sb-v": "0 2048 linear 259:3 0\n"}}
	if err := New(r.run).MakeReadOnly(context.Background(), "sb-v"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"dmsetup table sb-v",
		"dmsetup suspend --nolockfs sb-v",
		"dmsetup reload --readonly sb-v --table 0 2048 linear 259:3 0",
		"dmsetup resume sb-v",
	}
	if !slices.Equal(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
}
