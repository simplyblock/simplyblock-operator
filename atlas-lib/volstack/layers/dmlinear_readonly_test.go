// Tests for a dm-linear layer built read-only, for a volume every consumer only
// reads: the mapping it hands up refuses writes, whether the layer creates it
// or finds it made writable by an earlier driver.

package layers

import (
	"context"
	"slices"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
)

func readOnlyLayer(m *fakeMapper) *DMLinear {
	return NewDMLinear(DMLinearConfig{
		Name:     DMLinearName("vol-1"),
		Mapper:   m,
		ReadOnly: true,
		Resolve: func(path string) (blockdev.Device, error) {
			return blockdev.Device{Path: path, Name: "dm-7", Major: 253, Minor: 7}, nil
		},
	})
}

func TestAReadOnlyLayerCreatesAReadOnlyMapping(t *testing.T) {
	m := newFakeMapper()
	if _, err := readOnlyLayer(m).Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	if !m.readOnly["sb-vol-1"] {
		t.Fatalf("calls = %v, want the mapping created read-only", m.calls)
	}
}

// A volume staged before read-only staging existed holds a writable mapping. A
// restage makes it read-only in place, on the device the consumer already has.
func TestAReadOnlyLayerMakesAWritableMappingReadOnly(t *testing.T) {
	m := newFakeMapper()
	if _, err := dmLayer(m).Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnlyLayer(m).Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	if !m.readOnly["sb-vol-1"] {
		t.Fatalf("calls = %v, want the writable mapping made read-only", m.calls)
	}
	if slices.Contains(m.calls, "remove sb-vol-1") {
		t.Fatalf("calls = %v, want it converted, not recreated under its consumer", m.calls)
	}
}

func TestAReadOnlyMappingAlreadyReadOnlyIsLeftAlone(t *testing.T) {
	m := newFakeMapper()
	layer := readOnlyLayer(m)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	before := len(m.calls)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	if got := m.calls[before:]; len(got) != 0 {
		t.Fatalf("a second ensure ran %v, want nothing", got)
	}
}

func TestAWritableLayerCreatesAWritableMapping(t *testing.T) {
	m := newFakeMapper()
	if _, err := dmLayer(m).Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	if m.readOnly["sb-vol-1"] {
		t.Fatal("a writable layer created a read-only mapping")
	}
}
