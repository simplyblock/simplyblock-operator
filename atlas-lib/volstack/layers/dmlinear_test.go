package layers

import (
	"context"
	"errors"
	"slices"
	"strings"
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

// growDM reaches the layer through the runner's contract, which is the only way
// an expand reaches it: a layer that is not a Grower is walked past.
func growDM(t *testing.T, layer *DMLinear, below volstack.Artifact) (volstack.Artifact, error) {
	t.Helper()
	grower, ok := any(layer).(volstack.Grower)
	if !ok {
		t.Fatal("the dmLinear layer does not implement Grower, so an expand walks past it " +
			"and the mapping keeps the size it was created with")
	}
	return grower.Grow(context.Background(), below)
}

// The namespace grew: the mapping is extended to the device's new size, so the
// filesystem or the raw block consumer above sees the space. The device the
// mapping points at and the device above it stay the same.
func TestDMLinearGrowExtendsTheMappingToTheDeviceBelow(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	below := nvmeBelow("/dev/nvme1n1", 259, 3)
	if _, err := layer.Ensure(context.Background(), below); err != nil {
		t.Fatal(err)
	}
	m.sectors["/dev/nvme1n1"] = 4096

	own, err := growDM(t, layer, below)
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	got := m.tables["sb-vol-1"]
	if got.Sectors != 4096 {
		t.Fatalf("mapping is %d sectors after the grow, want the device's 4096", got.Sectors)
	}
	if got.Device != "259:3" {
		t.Fatalf("grow re-pointed the mapping at %s, want it on 259:3", got.Device)
	}
	if len(own.Devices) != 1 || own.Devices[0].Path != "/dev/mapper/sb-vol-1" {
		t.Fatalf("grow exposes %v, want the mapping", own.Devices)
	}
}

// kubelet reissues NodeExpandVolume after one that already succeeded. A mapping
// already the device's size is left alone: a reload suspends the consumer's I/O
// for nothing.
func TestDMLinearGrowIsConvergent(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	below := nvmeBelow("/dev/nvme1n1", 259, 3)
	if _, err := layer.Ensure(context.Background(), below); err != nil {
		t.Fatal(err)
	}
	m.sectors["/dev/nvme1n1"] = 4096
	for i := range 2 {
		if _, err := growDM(t, layer, below); err != nil {
			t.Fatalf("grow %d: %v", i+1, err)
		}
	}
	swaps := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, "swap ") {
			swaps++
		}
	}
	if swaps != 1 {
		t.Fatalf("reloaded the mapping %d times across two expands, want once: %v", swaps, m.calls)
	}
}

// A device that reads smaller than the mapping is never a reason to cut the
// mapping: the data past the new end would vanish under a mounted filesystem.
func TestDMLinearGrowNeverShrinksTheMapping(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	below := nvmeBelow("/dev/nvme1n1", 259, 3)
	if _, err := layer.Ensure(context.Background(), below); err != nil {
		t.Fatal(err)
	}
	m.sectors["/dev/nvme1n1"] = 1024

	if _, err := growDM(t, layer, below); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if got := m.tables["sb-vol-1"].Sectors; got != 2048 {
		t.Fatalf("mapping is %d sectors, want it kept at 2048", got)
	}
}

// An expand with no device below has nothing to size the mapping from.
func TestDMLinearGrowWithNothingBelowFails(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := growDM(t, layer, volstack.Artifact{}); err == nil {
		t.Fatal("growing over nothing must fail")
	}
}

// Regression: 2026-11-19-ext4-stays-aborted-behind-dmlinear (e2e run
// 44487645991). After total path loss the mapping points at a namespace the
// kernel removed, and the filesystem above it saw its I/O fail. The layer says
// so, which is what makes the heal remount the filesystem rather than keep it.
func TestDMLinearReportsAMappingWhoseNamespaceWasRemovedAsInterrupted(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	layer.cfg.Gone = func(device string) bool { return device == "259:3" }

	_, own, err := layer.Observe(context.Background(), nvmeBelow("/dev/nvme2n1", 259, 9))
	if err != nil {
		t.Fatal(err)
	}
	if !own.Interrupted {
		t.Fatal("a mapping over a removed namespace is not reported Interrupted")
	}
	if err := layer.Heal(context.Background(), nvmeBelow("/dev/nvme2n1", 259, 9), own); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if _, own, _ = layer.Observe(context.Background(), nvmeBelow("/dev/nvme2n1", 259, 9)); own.Interrupted {
		t.Fatal("still Interrupted after the mapping was re-pointed")
	}
}

// A namespace that moved while the old one is still on the host is the live
// subsystem flip, and the filesystem above it keeps serving through it.
func TestDMLinearAMovedNamespaceIsNotAnInterruption(t *testing.T) {
	m := newFakeMapper()
	layer := dmLayer(m)
	if _, err := layer.Ensure(context.Background(), nvmeBelow("/dev/nvme1n1", 259, 3)); err != nil {
		t.Fatal(err)
	}
	layer.cfg.Gone = func(string) bool { return false }
	if _, own, _ := layer.Observe(context.Background(), nvmeBelow("/dev/nvme4n2", 259, 11)); own.Interrupted {
		t.Fatal("a move without loss is reported Interrupted")
	}
}

// reconnectedFabric exposes the namespace a reconnect brought the volume back
// as, which is what the fabric layer hands upward after total path loss.
type reconnectedFabric struct{ dev blockdev.Device }

func (r reconnectedFabric) Name() string                                     { return "fabric" }
func (r reconnectedFabric) Release(context.Context, volstack.Artifact) error { return nil }
func (r reconnectedFabric) Destroy(context.Context, volstack.Artifact) error { return nil }
func (r reconnectedFabric) Ensure(context.Context, volstack.Artifact) (volstack.Artifact, error) {
	return volstack.Artifact{Devices: []blockdev.Device{r.dev}}, nil
}
func (r reconnectedFabric) Observe(context.Context, volstack.Artifact) (volstack.State, volstack.Artifact, error) {
	return volstack.StateReady, volstack.Artifact{Devices: []blockdev.Device{r.dev}}, nil
}

// unmountFirstMapper fails a test that re-points the mapping while the
// filesystem on it is still mounted.
type unmountFirstMapper struct {
	*fakeMapper
	fs *fakeFS
	t  *testing.T
}

func (u unmountFirstMapper) Swap(ctx context.Context, name, device string, sectors uint64) error {
	if u.fs.mountPoints[stagingPath] {
		u.t.Errorf("re-pointed %s while the filesystem on it was still mounted", name)
	}
	return u.fakeMapper.Swap(ctx, name, device, sectors)
}

// totalLossStack is an indirect filesystem stack the moment after total path
// loss: the mapping points at 259:3, which the kernel removed, the fabric
// brought the volume back as 259:9, and the dead ext4 is still mounted.
func totalLossStack(t *testing.T) (volstack.Plan, *fakeMapper, *fakeFS) {
	m := newFakeMapper()
	m.tables[DMLinearName("vol-1")] = devmapper.Target{Sectors: 2048, Device: "259:3"}
	fs := newFakeFS()
	fs.mountPoints[stagingPath] = true
	dm := NewDMLinear(DMLinearConfig{
		Name:   DMLinearName("vol-1"),
		Mapper: unmountFirstMapper{fakeMapper: m, fs: fs, t: t},
		Resolve: func(path string) (blockdev.Device, error) {
			return blockdev.Device{Path: path, Name: "dm-7", Major: 253, Minor: 7}, nil
		},
		Gone: func(device string) bool { return device == "259:3" },
	})
	fsLayer := newFS(t, fs, blockdev.Reading{Content: blockdev.ContentFilesystem, Type: "ext4"}, nil)
	fabric := reconnectedFabric{dev: blockdev.Device{Path: "/dev/nvme2n1", Major: 259, Minor: 9}}
	return volstack.Plan{fabric, dm, fsLayer}, m, fs
}

// Regression: 2026-11-19-ext4-stays-aborted-behind-dmlinear (e2e run
// 44487645991). The heal re-pointed the mapping under the mounted ext4 and kept
// the mount, whose journal had aborted during the outage, so every write after
// it failed read-only. The heal unmounts first and mounts the filesystem again
// over the re-pointed mapping, which replays its journal.
func TestTotalPathLossUnderTheIndirectionRemountsTheFilesystem(t *testing.T) {
	plan, m, fs := totalLossStack(t)
	if err := volstack.NewRunner(volstack.NewStore(t.TempDir())).Heal(context.Background(), "vol-1", plan); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if got := m.tables[DMLinearName("vol-1")].Device; got != "259:9" {
		t.Fatalf("mapping points at %s, want the reconnected namespace 259:9", got)
	}
	if !slices.Contains(fs.unmounted, stagingPath) {
		t.Fatalf("the filesystem was never unmounted: %v", fs.unmounted)
	}
	if len(fs.mounted) == 0 || fs.mounted[len(fs.mounted)-1].source != "/dev/mapper/sb-vol-1" {
		t.Fatalf("the filesystem was not mounted again over the mapping: %+v", fs.mounted)
	}
}

// A pod's bind mount shares the superblock, so unmounting the staging path under
// it would let the next mount pick the aborted filesystem up again. The heal
// waits instead, and touches neither the mount nor the mapping.
func TestTotalPathLossWaitsForTheFilesystemsOtherMounts(t *testing.T) {
	plan, m, fs := totalLossStack(t)
	fs.others = map[string][]string{stagingPath: {"/var/lib/kubelet/pods/a/volumes/v/mount"}}
	err := volstack.NewRunner(volstack.NewStore(t.TempDir())).Heal(context.Background(), "vol-1", plan)
	if !errors.Is(err, volstack.ErrHeldElsewhere) {
		t.Fatalf("Heal = %v, want ErrHeldElsewhere", err)
	}
	if !strings.Contains(err.Error(), "/var/lib/kubelet/pods/a/volumes/v/mount") {
		t.Errorf("the error does not name the other mount: %v", err)
	}
	if !fs.mountPoints[stagingPath] || m.tables[DMLinearName("vol-1")].Device != "259:3" {
		t.Error("the heal changed the stack while the filesystem was held elsewhere")
	}
}
