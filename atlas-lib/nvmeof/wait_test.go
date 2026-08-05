package nvmeof

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/nvme"
)

// fakeDevs is a nvme.DeviceResolver whose List returns one scripted snapshot
// per call, repeating the last one once the script runs out — modelling a
// kernel state that changes between polls.
type fakeDevs struct {
	snapshots [][]nvme.Device
	calls     int
	err       error
}

func (f *fakeDevs) List(context.Context) ([]nvme.Device, error) {
	if f.err != nil {
		return nil, f.err
	}
	i := f.calls
	f.calls++
	if i >= len(f.snapshots) {
		i = len(f.snapshots) - 1
	}
	return f.snapshots[i], nil
}

func (f *fakeDevs) ListWithSelector(ctx context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	all, err := f.List(ctx)
	if err != nil {
		return nil, err
	}
	return sel.Filter(all), nil
}

func (f *fakeDevs) ByUUID(context.Context, string) (nvme.Device, error) { return nvme.Device{}, nil }
func (f *fakeDevs) ByDevicePath(context.Context, string) (nvme.Device, error) {
	return nvme.Device{}, nil
}
func (f *fakeDevs) ByNamespace(context.Context, string, nvme.NamespaceID) (nvme.Device, error) {
	return nvme.Device{}, nil
}

func dev(subsysID, nqn, name, majorMinor string, nsid nvme.NamespaceID) nvme.Device {
	return nvme.Device{
		Namespace: nvme.Namespace{
			ID:         nsid,
			Name:       name,
			DevicePath: "/dev/" + name,
			Dev:        majorMinor,
			UUID:       "6dbb7d4e-2f1a-4a55-9d3c-1f2e3a4b5c6d",
		},
		Subsystem: nvme.Subsystem{ID: nvme.SubsystemID(subsysID), NQN: nqn},
	}
}

// reachableDev is dev() plus the state that makes a device usable: a live
// controller and an optimized ANA path.
func reachableDev(subsysID, nqn, name, majorMinor string, nsid nvme.NamespaceID) nvme.Device {
	d := dev(subsysID, nqn, name, majorMinor, nsid)
	d.Namespace.Paths = []nvme.Path{{Controller: "nvme0", ANAState: nvme.ANAOptimized, NSID: nsid}}
	d.Subsystem.Controllers = []nvme.Controller{{ID: "nvme0", State: "live"}}
	return d
}

// staleDev is a device the kernel still lists but that can serve no I/O — the
// leftover of an earlier connect to the same NQN.
func staleDev(subsysID, nqn, name, majorMinor string, nsid nvme.NamespaceID) nvme.Device {
	d := dev(subsysID, nqn, name, majorMinor, nsid)
	d.Namespace.Paths = []nvme.Path{{Controller: "nvme1", ANAState: nvme.ANAInaccessible, NSID: nsid}}
	d.Subsystem.Controllers = []nvme.Controller{{ID: "nvme1", State: "connecting"}}
	return d
}

func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestWaitForDevice_AppearsAfterConnect(t *testing.T) {
	d := dev("nvme-subsys0", "nqn.x", "nvme0n1", "259:1", 1)
	devs := &fakeDevs{snapshots: [][]nvme.Device{
		{}, // controller live, block device not visible yet
		{dev("nvme-subsys0", "nqn.other", "nvme1n1", "259:2", 1)}, // unrelated target
		{d},
	}}
	got, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace.DevicePath != "/dev/nvme0n1" {
		t.Errorf("device = %q, want /dev/nvme0n1", got.Namespace.DevicePath)
	}
}

func TestWaitForDevice_DuplicateEntriesForSameDevice(t *testing.T) {
	d := dev("nvme-subsys0", "nqn.x", "nvme0n1", "259:1", 1)
	devs := &fakeDevs{snapshots: [][]nvme.Device{{d, d}}}
	got, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.x"})
	if err != nil {
		t.Fatalf("two entries for one device: %v", err)
	}
	if got.Namespace.Dev != "259:1" {
		t.Errorf("dev = %q, want 259:1", got.Namespace.Dev)
	}
}

// A stale namespace from an earlier connect to the same NQN is a different
// device; the wait must not return either until the kernel has reaped it.
func TestWaitForDevice_WaitsOutStaleDuplicate(t *testing.T) {
	stale := dev("nvme-subsys0", "nqn.x", "nvme0n1", "259:1", 1)
	fresh := dev("nvme-subsys1", "nqn.x", "nvme2n1", "259:7", 1)
	devs := &fakeDevs{snapshots: [][]nvme.Device{
		{stale, fresh},
		{fresh},
	}}
	got, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace.Dev != "259:7" {
		t.Errorf("dev = %q, want the surviving device 259:7", got.Namespace.Dev)
	}
}

// When one of two rival devices can serve no I/O, it is the stale leftover and
// the wait resolves immediately instead of sitting out the kernel's reaping.
func TestWaitForDevice_SkipsUnreachableRival(t *testing.T) {
	devs := &fakeDevs{snapshots: [][]nvme.Device{{
		staleDev("nvme-subsys0", "nqn.x", "nvme0n1", "259:1", 1),
		reachableDev("nvme-subsys1", "nqn.x", "nvme2n1", "259:7", 1),
	}}}
	got, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace.Dev != "259:7" {
		t.Errorf("dev = %q, want the reachable 259:7", got.Namespace.Dev)
	}
	if devs.calls != 1 {
		t.Errorf("List called %d times, want 1 (no waiting needed)", devs.calls)
	}
}

// Reachability decides between candidates; it does not pick a favourite among
// equals. Two live-looking rivals — a stale head that has not lost its paths
// yet — must still be waited out rather than guessed at.
func TestWaitForDevice_WaitsWhenRivalsBothReachable(t *testing.T) {
	devs := &fakeDevs{snapshots: [][]nvme.Device{{
		reachableDev("nvme-subsys0", "nqn.x", "nvme0n1", "259:1", 1),
		reachableDev("nvme-subsys1", "nqn.x", "nvme2n1", "259:7", 1),
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	if _, err := WaitForDevice(ctx, devs, nvme.DeviceSelector{NQN: "nqn.x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
}

// A sole match is returned even when nothing about it is reachable yet:
// reachability narrows a match set, it never rejects the only candidate.
func TestWaitForDevice_SoleUnreachableMatchIsReturned(t *testing.T) {
	devs := &fakeDevs{snapshots: [][]nvme.Device{{
		staleDev("nvme-subsys0", "nqn.x", "nvme0n1", "259:1", 1),
	}}}
	got, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace.Dev != "259:1" {
		t.Errorf("dev = %q, want 259:1", got.Namespace.Dev)
	}
}

func TestWaitForDevice_TimesOutOnPersistentConflict(t *testing.T) {
	devs := &fakeDevs{snapshots: [][]nvme.Device{{
		dev("nvme-subsys0", "nqn.x", "nvme0n1", "259:1", 1),
		dev("nvme-subsys1", "nqn.x", "nvme2n1", "259:7", 1),
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	_, err := WaitForDevice(ctx, devs, nvme.DeviceSelector{NQN: "nqn.x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	for _, want := range []string{"different devices", "/dev/nvme0n1", "/dev/nvme2n1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

// Distinct namespaces of one subsystem cannot be disambiguated by waiting —
// the selector is under-specified, so fail immediately.
func TestWaitForDevice_AmbiguousSelectorFailsFast(t *testing.T) {
	devs := &fakeDevs{snapshots: [][]nvme.Device{{
		dev("nvme-subsys0", "nqn.ns", "nvme0n1", "259:1", 1),
		dev("nvme-subsys0", "nqn.ns", "nvme0n2", "259:2", 2),
	}}}
	_, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.ns"})
	if !errors.Is(err, errAmbiguousSelector) {
		t.Fatalf("err = %v, want an ambiguous-selector error", err)
	}
	if devs.calls != 1 {
		t.Errorf("List called %d times, want 1 (no polling)", devs.calls)
	}
}

func TestWaitForDevice_NSIDDisambiguates(t *testing.T) {
	devs := &fakeDevs{snapshots: [][]nvme.Device{{
		dev("nvme-subsys0", "nqn.ns", "nvme0n1", "259:1", 1),
		dev("nvme-subsys0", "nqn.ns", "nvme0n2", "259:2", 2),
	}}}
	got, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.ns", NSID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace.ID != 2 {
		t.Errorf("nsid = %d, want 2", got.Namespace.ID)
	}
}

func TestWaitForDevice_UUIDDisambiguates(t *testing.T) {
	a := dev("nvme-subsys0", "nqn.ns", "nvme0n1", "259:1", 1)
	b := dev("nvme-subsys0", "nqn.ns", "nvme0n2", "259:2", 2)
	b.Namespace.UUID = "AAAAAAAA-2f1a-4a55-9d3c-1f2e3a4b5c6d" // control plane may upper-case it
	devs := &fakeDevs{snapshots: [][]nvme.Device{{a, b}}}

	got, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{
		NQN:  "nqn.ns",
		UUID: "aaaaaaaa-2f1a-4a55-9d3c-1f2e3a4b5c6d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace.ID != 2 {
		t.Errorf("nsid = %d, want the uuid-matched namespace 2", got.Namespace.ID)
	}
}

// Without a major:minor from sysfs, identity comes from the resolved device
// node — so a symlink and its target count as one device.
func TestWaitForDevice_ResolvesSymlinkedDeviceNodes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nvme0n1")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "by-id-alias")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	byName := dev("nvme-subsys0", "nqn.x", "nvme0n1", "", 1)
	byName.Namespace.DevicePath = target
	byID := byName
	byID.Namespace.DevicePath = link

	devs := &fakeDevs{snapshots: [][]nvme.Device{{byName, byID}}}
	if _, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.x"}); err != nil {
		t.Fatalf("symlink and target treated as different devices: %v", err)
	}
}

func TestWaitForDevice_UnresolvableNodeIsRetried(t *testing.T) {
	gone := dev("nvme-subsys0", "nqn.x", "nvme0n1", "", 1)
	gone.Namespace.DevicePath = filepath.Join(t.TempDir(), "does-not-exist")
	fresh := dev("nvme-subsys1", "nqn.x", "nvme2n1", "259:7", 1)
	devs := &fakeDevs{snapshots: [][]nvme.Device{
		{gone, fresh},
		{fresh},
	}}
	got, err := WaitForDevice(waitCtx(t), devs, nvme.DeviceSelector{NQN: "nqn.x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace.Dev != "259:7" {
		t.Errorf("dev = %q, want 259:7", got.Namespace.Dev)
	}
}

func TestWaitForDevice_EmptyNQN(t *testing.T) {
	if _, err := WaitForDevice(waitCtx(t), &fakeDevs{}, nvme.DeviceSelector{}); err == nil {
		t.Error("empty NQN accepted")
	}
}

func TestConnectDevice(t *testing.T) {
	d := dev("nvme-subsys0", "nqn.x", "nvme0n1", "259:1", 1)

	t.Run("connects then returns the device", func(t *testing.T) {
		connected := false
		c := &FabricsConnector{
			poll: time.Millisecond,
			subs: fakeSubs{byNQN: func(_ context.Context, nqn string) (nvme.Subsystem, error) {
				if !connected {
					return notFound()
				}
				return liveSub(nqn, "10.0.0.1")
			}},
			connect: func(context.Context, string) (string, error) { connected = true; return "", nil },
		}
		devs := &fakeDevs{snapshots: [][]nvme.Device{{}, {d}}}

		got, err := ConnectDevice(waitCtx(t), c, devs, Target{NQN: "nqn.x", Address: "10.0.0.1"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got.Namespace.DevicePath != "/dev/nvme0n1" {
			t.Errorf("device = %q, want /dev/nvme0n1", got.Namespace.DevicePath)
		}
	})

	t.Run("connect failure is not waited out", func(t *testing.T) {
		c := &FabricsConnector{
			poll: time.Millisecond,
			subs: fakeSubs{byNQN: func(context.Context, string) (nvme.Subsystem, error) { return notFound() }},
			connect: func(context.Context, string) (string, error) {
				return "", errors.New("connection refused")
			},
		}
		devs := &fakeDevs{snapshots: [][]nvme.Device{{d}}}

		_, err := ConnectDevice(waitCtx(t), c, devs, Target{NQN: "nqn.x", Address: "a"}, 0)
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("err = %v, want the connect error", err)
		}
		if devs.calls != 0 {
			t.Errorf("List called %d times after a failed connect, want 0", devs.calls)
		}
	})
}

func TestConnectMultipathDevice(t *testing.T) {
	// Two namespaces of one multi-namespace subsystem: staging the wrong one is
	// the mistake the NSID selector prevents.
	ns1 := dev("nvme-subsys0", testNQN, "nvme0n1", "259:1", 1)
	ns2 := dev("nvme-subsys0", testNQN, "nvme0n2", "259:2", 2)

	t.Run("attaches every path, then returns the selected namespace", func(t *testing.T) {
		f := &fabric{}
		devs := &fakeDevs{snapshots: [][]nvme.Device{{}, {ns1, ns2}}}

		got, results, err := ConnectMultipathDevice(waitCtx(t), f.connector(), devs,
			targets("10.0.0.1", "10.0.0.2", "10.0.0.3"), 2)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.order, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}) {
			t.Errorf("attach order = %v, want all three in priority order", f.order)
		}
		if len(results) != 3 {
			t.Errorf("results = %d, want one per path", len(results))
		}
		if got.Namespace.DevicePath != "/dev/nvme0n2" {
			t.Errorf("device = %q, want /dev/nvme0n2 (nsid 2)", got.Namespace.DevicePath)
		}
	})

	t.Run("a degraded volume still stages", func(t *testing.T) {
		f := &fabric{fail: map[string]error{"10.0.0.1": errors.New("connection refused")}}
		devs := &fakeDevs{snapshots: [][]nvme.Device{{ns1}}}

		got, results, err := ConnectMultipathDevice(waitCtx(t), f.connector(), devs,
			targets("10.0.0.1", "10.0.0.2"), 0)
		if err != nil {
			t.Fatalf("err = %v, want nil: the secondary path came up", err)
		}
		if got.Namespace.DevicePath != "/dev/nvme0n1" {
			t.Errorf("device = %q, want /dev/nvme0n1", got.Namespace.DevicePath)
		}
		if results[0].Live || results[1].Live == false {
			t.Errorf("results = %+v, want the primary failed and the secondary live", results)
		}
	})

	t.Run("no path up returns the per-path reasons", func(t *testing.T) {
		f := &fabric{fail: map[string]error{
			"10.0.0.1": errors.New("connection refused"),
			"10.0.0.2": errors.New("no route to host"),
		}}
		devs := &fakeDevs{snapshots: [][]nvme.Device{{}}}

		_, results, err := ConnectMultipathDevice(waitCtx(t), f.connector(), devs,
			targets("10.0.0.1", "10.0.0.2"), 0)
		if err == nil {
			t.Error("err = nil, want an error when nothing came up")
		}
		if len(results) != 2 || results[0].Err == nil || results[1].Err == nil {
			t.Errorf("results = %+v, want both failures recorded", results)
		}
	})

	t.Run("paths up but no device", func(t *testing.T) {
		f := &fabric{}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		devs := &fakeDevs{snapshots: [][]nvme.Device{{}}} // never shows up

		_, results, err := ConnectMultipathDevice(ctx, f.connector(), devs, targets("10.0.0.1"), 0)
		if err == nil {
			t.Error("err = nil, want the device wait to fail")
		}
		if len(results) != 1 || !results[0].Live {
			t.Errorf("results = %+v, want the path reported live", results)
		}
	})

	t.Run("no targets", func(t *testing.T) {
		f := &fabric{}
		if _, _, err := ConnectMultipathDevice(waitCtx(t), f.connector(), &fakeDevs{}, nil, 0); err == nil {
			t.Error("err = nil, want an error for an empty target list")
		}
	})
}
