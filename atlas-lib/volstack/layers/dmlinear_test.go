package layers

import (
	"context"
	"errors"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/devmapper"
	"github.com/simplyblock/atlas/volstack"
)

// fakeMapper keeps mappings in memory and records the verbs run against them.
type fakeMapper struct {
	tables  map[string]devmapper.Target
	sectors map[string]uint64
	calls   []string
	swapErr error
}

func newFakeMapper() *fakeMapper {
	return &fakeMapper{tables: map[string]devmapper.Target{}, sectors: map[string]uint64{}}
}

func (f *fakeMapper) Table(_ context.Context, name string) (devmapper.Target, error) {
	t, ok := f.tables[name]
	if !ok {
		return devmapper.Target{}, devmapper.ErrNotFound
	}
	return t, nil
}

func (f *fakeMapper) Sectors(_ context.Context, device string) (uint64, error) {
	if n, ok := f.sectors[device]; ok {
		return n, nil
	}
	return 2048, nil
}

func (f *fakeMapper) Create(_ context.Context, name, device string, sectors uint64) error {
	f.calls = append(f.calls, "create "+name+" "+device)
	f.tables[name] = devmapper.Target{Sectors: sectors, Device: device}
	return nil
}

func (f *fakeMapper) Swap(_ context.Context, name, device string, sectors uint64) error {
	f.calls = append(f.calls, "swap "+name+" "+device)
	if f.swapErr != nil {
		return f.swapErr
	}
	f.tables[name] = devmapper.Target{Sectors: sectors, Device: device}
	return nil
}

func (f *fakeMapper) Remove(_ context.Context, name string) error {
	f.calls = append(f.calls, "remove "+name)
	delete(f.tables, name)
	return nil
}

func nvmeBelow(path string, major, minor uint32) volstack.Artifact {
	return volstack.Artifact{Devices: []blockdev.Device{{Path: path, Major: major, Minor: minor}}}
}

func dmLayer(m *fakeMapper) *DMLinear {
	return NewDMLinear(DMLinearConfig{
		Name:   DMLinearName("vol-1"),
		Mapper: m,
		Resolve: func(path string) (blockdev.Device, error) {
			return blockdev.Device{Path: path, Name: "dm-7", Major: 253, Minor: 7}, nil
		},
	})
}

func TestDMLinearEnsureMapsTheDeviceBelowAndExposesTheMapping(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	own, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(m.calls) != 1 || m.calls[0] != "create sb-vol-1 259:3" {
		t.Fatalf("calls = %v, want one create over 259:3", m.calls)
	}
	if own.Devices[0].Path != "/dev/mapper/sb-vol-1" {
		t.Fatalf("exposes %s, want the mapping", own.Devices[0].Path)
	}
	state, _, _ := layer.Observe(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3))
	if state != volstack.StateReady {
		t.Fatalf("state after ensure = %v, want Ready", state)
	}
}

// The namespace moved to another subsystem: the fabric below now exposes a
// different device. The mapping is reported stale and the heal re-points it
// at the new device; the device above keeps its identity.
func TestDMLinearHealRepointsAMappingWhoseNamespaceMoved(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	moved := nvmeBelow("/dev/nvme4n2", 259, 11)
	state, own, err := layer.Observe(context.Background(), moved)
	if err != nil || state != volstack.StatePartial {
		t.Fatalf("observe after the move = %v %v, want Partial", state, err)
	}
	if healthy, _ := layer.Healthy(context.Background(), own); healthy {
		t.Fatal("a mapping pointing at the old namespace is not healthy")
	}
	if err := layer.Heal(context.Background(), moved, own); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if got := m.tables["sb-vol-1"].Device; got != "259:11" {
		t.Fatalf("mapping points at %s, want the new namespace 259:11", got)
	}
	if healthy, _ := layer.Healthy(context.Background(), own); !healthy {
		t.Fatal("healthy after the swap")
	}
	if own.Devices[0].Path != "/dev/mapper/sb-vol-1" {
		t.Fatalf("the device above changed: %s", own.Devices[0].Path)
	}
}

func TestDMLinearHealWithNothingBelowFailsAndLeavesTheMapping(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	if err := layer.Heal(context.Background(), volstack.Artifact{}, volstack.Artifact{}); err == nil {
		t.Fatal("re-pointing at nothing must fail")
	}
	if got := m.tables["sb-vol-1"].Device; got != "259:3" {
		t.Fatalf("a failed heal must leave the mapping, now %s", got)
	}
}

func TestDMLinearAFailedSwapKeepsTheLayerUnhealthy(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	moved := nvmeBelow("/dev/nvme4n2", 259, 11)
	_, own, _ := layer.Observe(context.Background(), moved)
	m.swapErr = errors.New("reload refused")
	if err := layer.Heal(context.Background(), moved, own); err == nil {
		t.Fatal("expected the swap error")
	}
	if healthy, _ := layer.Healthy(context.Background(), own); healthy {
		t.Fatal("still stale after a failed swap")
	}
}

func TestDMLinearReleaseRemovesTheMappingAndDestroyKeepsNothing(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	if err := layer.Release(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.tables["sb-vol-1"]; ok {
		t.Fatal("release must remove the mapping")
	}
	if err := layer.Destroy(context.Background(), volstack.Artifact{}); err != nil {
		t.Fatal(err)
	}
}
