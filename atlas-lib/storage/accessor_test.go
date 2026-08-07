package storage

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/nvme"
)

// fakeDevices is a nvme.DeviceResolver over a snapshot the test can change
// between calls, which is how these tests tell a re-scan from a read of the
// snapshot the device was taken from.
type fakeDevices struct {
	devs  []nvme.Device
	calls int
}

func (f *fakeDevices) List(context.Context) ([]nvme.Device, error) { return f.devs, nil }

func (f *fakeDevices) ListWithSelector(_ context.Context, sel nvme.DeviceSelector) ([]nvme.Device, error) {
	f.calls++
	return sel.Filter(f.devs), nil
}

func (f *fakeDevices) ByUUID(ctx context.Context, uuid string) (nvme.Device, error) {
	return f.first(ctx, nvme.DeviceSelector{UUID: uuid})
}

func (f *fakeDevices) ByDevicePath(ctx context.Context, devicePath string) (nvme.Device, error) {
	return f.first(ctx, nvme.DeviceSelector{DevicePath: devicePath})
}

func (f *fakeDevices) ByNamespace(ctx context.Context, nqn string, nsid nvme.NamespaceID) (nvme.Device, error) {
	return f.first(ctx, nvme.DeviceSelector{NQN: nqn, NSID: nsid})
}

func (f *fakeDevices) first(ctx context.Context, sel nvme.DeviceSelector) (nvme.Device, error) {
	matched, err := f.ListWithSelector(ctx, sel)
	if err != nil {
		return nvme.Device{}, err
	}
	if len(matched) == 0 {
		return nvme.Device{}, errs.ErrNotFound
	}
	return matched[0], nil
}

// fakeSubsystems is a nvme.SubsystemResolver over a fixed snapshot.
type fakeSubsystems struct {
	subsystems []nvme.Subsystem
}

func (f *fakeSubsystems) List(context.Context) ([]nvme.Subsystem, error) {
	return f.subsystems, nil
}

func (f *fakeSubsystems) ByNQN(_ context.Context, nqn string) (nvme.Subsystem, error) {
	for _, s := range f.subsystems {
		if s.NQN == nqn {
			return s, nil
		}
	}
	return nvme.Subsystem{}, errs.ErrNotFound
}

func dev(name, uuid string) nvme.Device {
	return nvme.Device{
		Namespace: nvme.Namespace{
			Name:       name,
			SysfsPath:  "/sys/class/nvme-subsystem/nvme-subsys0/" + name,
			DevicePath: "/dev/" + name,
			UUID:       uuid,
		},
	}
}

func names(devs []nvme.Device) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.Namespace.Name
	}
	return out
}

// devicesOf returns one Device per namespace of s, the way a scan reports them.
func devicesOf(s nvme.Subsystem) []nvme.Device {
	out := make([]nvme.Device, 0, len(s.Namespaces))
	for _, ns := range s.Namespaces {
		out = append(out, nvme.Device{Namespace: ns, Subsystem: s})
	}
	return out
}

var sharedSubsys = nvme.Subsystem{
	ID:  "nvme-subsys0",
	NQN: "nqn.test:shared",
	Namespaces: []nvme.Namespace{
		{ID: 1, Name: "nvme0n1", UUID: "vol-a", DevicePath: "/dev/nvme0n1"},
		{ID: 2, Name: "nvme0n2", UUID: "vol-b", DevicePath: "/dev/nvme0n2"},
		{ID: 3, Name: "nvme0n3", UUID: "vol-c", DevicePath: "/dev/nvme0n3"},
	},
}

// The whole reason these live on Accessor rather than being read off the device:
// the device was resolved when it was its volume's only block device, and the
// answer has to reflect the path that showed up since.
func TestSiblingsRescan(t *testing.T) {
	const volA = "fee75e72-1291-4193-8357-3e228ced6c49"
	a1, a2 := dev("nvme0n1", volA), dev("nvme1n1", volA)

	devices := &fakeDevices{devs: []nvme.Device{a1, dev("nvme2n1", "other")}}
	s := Accessor{DeviceResolver: devices}
	ctx := context.Background()

	got, err := s.Siblings(ctx, a1)
	if err != nil || len(got) != 0 {
		t.Fatalf("Siblings = %v, %v; want none, nil", names(got), err)
	}

	devices.devs = append(devices.devs, a2)

	got, err = s.Siblings(ctx, a1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got), []string{"nvme1n1"}) {
		t.Errorf("Siblings = %v, want [nvme1n1]", names(got))
	}

	has, err := s.HasSiblings(ctx, a1)
	if err != nil || !has {
		t.Errorf("HasSiblings = %t, %v; want true, nil", has, err)
	}
}

// The teardown case: a namespace joining the subsystem after the device was
// resolved is exactly the one that makes disconnecting it destructive.
func TestCoTenantsRescan(t *testing.T) {
	alone := nvme.Subsystem{
		ID: sharedSubsys.ID, NQN: sharedSubsys.NQN,
		Namespaces: sharedSubsys.Namespaces[:1],
	}
	stale := devicesOf(alone)[0]

	devices := &fakeDevices{devs: devicesOf(alone)}
	s := Accessor{DeviceResolver: devices}
	ctx := context.Background()

	if has, err := s.HasCoTenants(ctx, stale); has || err != nil {
		t.Fatalf("HasCoTenants = %t, %v; want false, nil while alone on the subsystem", has, err)
	}

	devices.devs = devicesOf(sharedSubsys) // a namespaced sibling volume showed up

	has, err := s.HasCoTenants(ctx, stale)
	if err != nil || !has {
		t.Fatalf("HasCoTenants = %t, %v; want true, nil", has, err)
	}
	got, err := s.CoTenants(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got), []string{"nvme0n2", "nvme0n3"}) {
		t.Errorf("CoTenants = %v, want the other two namespaces", names(got))
	}
}

// A device with nothing to key on cannot have an answer scanned for it, so the
// scan is skipped rather than run and misinterpreted.
func TestRescanSkippedWithoutAnIdentity(t *testing.T) {
	devices := &fakeDevices{devs: devicesOf(sharedSubsys)}
	s := Accessor{DeviceResolver: devices}
	ctx := context.Background()

	if has, err := s.HasSiblings(ctx, dev("nvme0n1", "")); has || err != nil {
		t.Errorf("HasSiblings without a uuid = %t, %v; want false, nil", has, err)
	}
	if has, err := s.HasCoTenants(ctx, dev("nvme0n1", "vol-a")); has || err != nil {
		t.Errorf("HasCoTenants without a subsystem nqn = %t, %v; want false, nil", has, err)
	}
	if devices.calls != 0 {
		t.Errorf("resolver called %d times, want none", devices.calls)
	}
}

// Answering "nothing alongside" from an empty Accessor would read as permission
// to tear the subsystem down.
func TestRescanWithoutAResolverFails(t *testing.T) {
	var s Accessor
	ctx := context.Background()

	if _, err := s.Siblings(ctx, dev("nvme0n1", "vol-a")); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("Siblings on an empty Accessor = %v, want errs.ErrUnsupported", err)
	}
	if _, err := s.HasCoTenants(ctx, devicesOf(sharedSubsys)[0]); !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("HasCoTenants on an empty Accessor = %v, want errs.ErrUnsupported", err)
	}
}

// The flat lookups must be the resolvers' own answers, not a reimplementation
// of them — so each is checked against calling the resolver directly.
func TestLookupsDelegateToTheResolvers(t *testing.T) {
	subsystems := &fakeSubsystems{subsystems: []nvme.Subsystem{sharedSubsys}}
	devices := &fakeDevices{devs: devicesOf(sharedSubsys)}
	s := Accessor{SubsystemResolver: subsystems, DeviceResolver: devices}
	ctx := context.Background()

	t.Run("ListSubsystems", func(t *testing.T) {
		got, err := s.ListSubsystems(ctx)
		if err != nil {
			t.Fatalf("ListSubsystems: %v", err)
		}
		if len(got) != 1 || got[0].NQN != sharedSubsys.NQN {
			t.Errorf("ListSubsystems = %+v, want the one subsystem", got)
		}
	})

	t.Run("SubsystemByNQN", func(t *testing.T) {
		got, err := s.SubsystemByNQN(ctx, sharedSubsys.NQN)
		if err != nil {
			t.Fatalf("SubsystemByNQN: %v", err)
		}
		if got.NQN != sharedSubsys.NQN {
			t.Errorf("SubsystemByNQN = %s, want %s", got.NQN, sharedSubsys.NQN)
		}
	})

	t.Run("ListDevices", func(t *testing.T) {
		got, err := s.ListDevices(ctx)
		if err != nil {
			t.Fatalf("ListDevices: %v", err)
		}
		if !slices.Equal(names(got), []string{"nvme0n1", "nvme0n2", "nvme0n3"}) {
			t.Errorf("ListDevices = %v, want all three namespaces", names(got))
		}
	})

	t.Run("ListDevicesBySelector", func(t *testing.T) {
		got, err := s.ListDevicesBySelector(ctx, nvme.DeviceSelector{UUID: "vol-b"})
		if err != nil {
			t.Fatalf("ListDevicesBySelector: %v", err)
		}
		if !slices.Equal(names(got), []string{"nvme0n2"}) {
			t.Errorf("ListDevicesBySelector = %v, want [nvme0n2]", names(got))
		}
	})

	t.Run("DeviceByUUID", func(t *testing.T) {
		got, err := s.DeviceByUUID(ctx, "vol-c")
		if err != nil {
			t.Fatalf("DeviceByUUID: %v", err)
		}
		if got.Namespace.Name != "nvme0n3" {
			t.Errorf("DeviceByUUID = %s, want nvme0n3", got.Namespace.Name)
		}
	})

	t.Run("DeviceByPath", func(t *testing.T) {
		if _, err := s.DeviceByPath(ctx, "/dev/nvme0n1"); err != nil {
			t.Fatalf("DeviceByPath: %v", err)
		}
	})

	t.Run("DeviceByNamespace", func(t *testing.T) {
		got, err := s.DeviceByNamespace(ctx, sharedSubsys.NQN, 2)
		if err != nil {
			t.Fatalf("DeviceByNamespace: %v", err)
		}
		if got.Namespace.ID != 2 {
			t.Errorf("DeviceByNamespace = nsid %d, want 2", got.Namespace.ID)
		}
	})
}

// An absent facet is reported the same way wherever it is met, and never as an
// empty result — every entry point goes through the same guard.
func TestLookupsWithoutAResolverFail(t *testing.T) {
	var s Accessor
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"ListSubsystems": func() error { _, err := s.ListSubsystems(ctx); return err },
		"SubsystemByNQN": func() error { _, err := s.SubsystemByNQN(ctx, "nqn.test:x"); return err },
		"ListDevices":    func() error { _, err := s.ListDevices(ctx); return err },
		"ListDevicesBySelector": func() error {
			_, err := s.ListDevicesBySelector(ctx, nvme.DeviceSelector{})
			return err
		},
		"DeviceByUUID":      func() error { _, err := s.DeviceByUUID(ctx, "vol-a"); return err },
		"DeviceByPath":      func() error { _, err := s.DeviceByPath(ctx, "/dev/nvme0n1"); return err },
		"DeviceByNamespace": func() error { _, err := s.DeviceByNamespace(ctx, "nqn.test:x", 1); return err },
	} {
		if err := call(); !errors.Is(err, errs.ErrUnsupported) {
			t.Errorf("%s on an empty Accessor = %v, want errs.ErrUnsupported", name, err)
		}
	}
}

func TestLocalFillsBothResolvers(t *testing.T) {
	s := Local(nvme.SysfsConfig{SysRoot: t.TempDir(), DevRoot: t.TempDir()})
	if s.SubsystemResolver == nil {
		t.Error("Local left Subsystems unset")
	}
	if s.DeviceResolver == nil {
		t.Error("Local left Devices unset")
	}
}
