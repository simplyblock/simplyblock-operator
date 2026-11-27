// Tests for a dm-linear layer built read-only, for a volume every consumer only
// reads: the mapping it hands up refuses writes, whether the layer creates it
// or finds it made writable by an earlier driver.

package layers

import (
	"context"
	"slices"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/volstack"
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

// healAsTheRunnerDoes is the runner's pass over one layer at publish: observe,
// ask whether it is healthy, and heal it only if not.
func healAsTheRunnerDoes(t *testing.T, layer *DMLinear, below volstack.Artifact) {
	t.Helper()
	ctx := context.Background()
	_, own, err := layer.Observe(ctx, below)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := layer.Healthy(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	if !healthy {
		if err := layer.Heal(ctx, below, own); err != nil {
			t.Fatal(err)
		}
	}
}

// Regression: #989 review (dm-linear heal). NodePublishVolume heals the stack
// rather than ensuring it, and the heal never looked at the mapping's mode:
// Healthy answered only whether the mapping points at the device below, and
// Heal's re-point kept whatever mode the mapping had. A ReadOnlyMany volume
// whose mapping an earlier driver left writable therefore stayed writable on
// every republish, whether the mapping pointed at the right device or not.
func TestAHealMakesAReadOnlyLayersWritableMappingReadOnly(t *testing.T) {
	for name, writableOn := range map[string]volstack.Artifact{
		"mapping on the device below":  nvmeBelow("/dev/nvme1n1", 259, 3),
		"mapping on a replaced device": nvmeBelow("/dev/nvme2n1", 259, 9),
	} {
		t.Run(name, func(t *testing.T) {
			m := newFakeMapper()
			if _, err := dmLayer(m).Ensure(context.Background(), writableOn); err != nil {
				t.Fatal(err)
			}
			healAsTheRunnerDoes(t, readOnlyLayer(m), nvmeBelow("/dev/nvme1n1", 259, 3))
			if !m.readOnly["sb-vol-1"] {
				t.Fatalf("calls = %v, want the mapping read-only after the heal", m.calls)
			}
		})
	}
}
