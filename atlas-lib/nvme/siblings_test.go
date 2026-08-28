package nvme

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

// dev builds a Device with the given namespace name and UUID. The sysfs path is
// derived from the name so distinct names are distinct identities.
func dev(name, uuid string) Device {
	return Device{
		Namespace: Namespace{
			Name:       name,
			SysfsPath:  "/sys/class/nvme-subsystem/nvme-subsys0/" + name,
			DevicePath: "/dev/" + name,
			UUID:       uuid,
		},
	}
}

func names(devs []Device) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.Namespace.Name
	}
	return out
}

func TestSiblings(t *testing.T) {
	const volA = "fee75e72-1291-4193-8357-3e228ced6c49"
	const volB = "73806533-c8f5-4c09-ae1b-db287f3bd91d"

	a1 := dev("nvme0n1", volA) // volume A, path 1
	a2 := dev("nvme1n1", volA) // volume A, path 2 (multipath disabled)
	b1 := dev("nvme2n1", volB) // volume B
	all := []Device{a1, a2, b1}

	t.Run("returns other devices with same uuid", func(t *testing.T) {
		got := names(Siblings(a1, all))
		if len(got) != 1 || got[0] != "nvme1n1" {
			t.Errorf("siblings of a1 = %v, want [nvme1n1]", got)
		}
	})

	t.Run("excludes the device itself", func(t *testing.T) {
		for _, d := range Siblings(a1, all) {
			if d.Namespace.SysfsPath == a1.Namespace.SysfsPath {
				t.Errorf("siblings must not include the device itself")
			}
		}
	})

	t.Run("single-head volume (native multipath) has no siblings", func(t *testing.T) {
		if got := Siblings(b1, all); len(got) != 0 {
			t.Errorf("siblings of b1 = %v, want none", names(got))
		}
	})

	t.Run("device need not be present in the list", func(t *testing.T) {
		a3 := dev("nvme9n1", volA) // another path to A, not in `all`
		got := names(Siblings(a3, all))
		if len(got) != 2 {
			t.Errorf("siblings of a3 = %v, want both a1 and a2", got)
		}
	})

	t.Run("no uuid yields nil", func(t *testing.T) {
		if got := Siblings(dev("nvme0n1", ""), all); got != nil {
			t.Errorf("siblings for uuid-less device = %v, want nil", names(got))
		}
	})

	t.Run("the method resolves and matches the pure form", func(t *testing.T) {
		r := &fakeDeviceResolver{devs: all}
		for _, d := range []Device{a1, a2, b1, dev("nvme9n1", volA), dev("nvme0n1", "")} {
			want := names(Siblings(d, all))
			got, err := d.WithResolver(r).Siblings(context.Background())
			if err != nil {
				t.Fatalf("Siblings(%s): %v", d.Namespace.Name, err)
			}
			if !slices.Equal(names(got), want) {
				t.Errorf("Siblings(%s) = %v, want %v", d.Namespace.Name, names(got), want)
			}
		}
	})

	t.Run("HasSiblings agrees with Siblings", func(t *testing.T) {
		r := &fakeDeviceResolver{devs: all}
		for _, d := range []Device{a1, a2, b1, dev("nvme9n1", volA), dev("nvme0n1", "")} {
			want := len(Siblings(d, all)) > 0
			got, err := d.WithResolver(r).HasSiblings(context.Background())
			if err != nil {
				t.Fatalf("HasSiblings(%s): %v", d.Namespace.Name, err)
			}
			if got != want {
				t.Errorf("HasSiblings(%s) = %t, want %t", d.Namespace.Name, got, want)
			}
		}
	})

	t.Run("HasSiblings skips the scan without a uuid", func(t *testing.T) {
		r := &countingResolver{fakeDeviceResolver{devs: all}, 0}
		d := dev("nvme0n1", "").WithResolver(r)
		if got, err := d.HasSiblings(context.Background()); got || err != nil {
			t.Errorf("HasSiblings for uuid-less device = %t, %v; want false, nil", got, err)
		}
		if r.calls != 0 {
			t.Errorf("resolver called %d times, want none", r.calls)
		}
	})

	t.Run("HasSiblings needs a resolver", func(t *testing.T) {
		_, err := a1.HasSiblings(context.Background()) // never bound
		if !errors.Is(err, errs.ErrUnsupported) {
			t.Errorf("HasSiblings on an unbound device = %v, want errs.ErrUnsupported", err)
		}
	})
}

// devicesOf returns one Device per namespace of s, the way a scan reports them.
func devicesOf(s Subsystem) []Device {
	out := make([]Device, 0, len(s.Namespaces))
	for _, ns := range s.Namespaces {
		out = append(out, Device{Namespace: ns, Subsystem: s})
	}
	return out
}

// A multi-namespace subsystem with three volumes.
var sharedSubsys = Subsystem{
	ID:  "nvme-subsys0",
	NQN: "nqn.test:shared",
	Namespaces: []Namespace{
		{ID: 1, Name: "nvme0n1", UUID: "vol-a"},
		{ID: 2, Name: "nvme0n2", UUID: "vol-b"},
		{ID: 3, Name: "nvme0n3", UUID: "vol-c"},
	},
}

func TestCoTenants(t *testing.T) {
	all := devicesOf(sharedSubsys)
	d := all[0] // nsid 1, vol-a

	got := CoTenants(d, all)
	if len(got) != 2 {
		t.Fatalf("CoTenants = %d, want 2", len(got))
	}
	for _, ct := range got {
		if ct.Namespace.ID == d.Namespace.ID {
			t.Error("CoTenants must exclude the device's own namespace")
		}
		if ct.Subsystem.NQN != sharedSubsys.NQN {
			t.Errorf("co-tenant subsystem NQN = %q, want %q", ct.Subsystem.NQN, sharedSubsys.NQN)
		}
	}

	t.Run("volumes on another subsystem are not co-tenants", func(t *testing.T) {
		other := Subsystem{ID: "nvme-subsys9", NQN: "nqn.test:other", Namespaces: []Namespace{
			{ID: 1, Name: "nvme9n1", UUID: "vol-z"},
			{ID: 2, Name: "nvme9n2", UUID: "vol-zz"},
		}}
		mixed := append(devicesOf(sharedSubsys), devicesOf(other)...)
		if got := names(CoTenants(d, mixed)); len(got) != 2 {
			t.Errorf("CoTenants across subsystems = %v, want only the two on d's subsystem", got)
		}
	})

	t.Run("single-namespace subsystem has none", func(t *testing.T) {
		solo := Subsystem{ID: "nvme-subsys1", NQN: "nqn.test:solo",
			Namespaces: []Namespace{{ID: 1, Name: "nvme1n1", UUID: "vol-x"}}}
		soloDevs := devicesOf(solo)
		if ct := CoTenants(soloDevs[0], soloDevs); len(ct) != 0 {
			t.Errorf("single-namespace CoTenants = %v, want none", names(ct))
		}
	})

	t.Run("the same namespace per controller is one volume", func(t *testing.T) {
		// Without a multipath head a namespace appears once per controller,
		// and those repeats are the same volume, not another tenant.
		repeated := Subsystem{ID: "nvme-subsys2", NQN: "nqn.test:repeated", Namespaces: []Namespace{
			{ID: 1, Name: "nvme2c0n1", UUID: "vol-y"},
			{ID: 1, Name: "nvme2c1n1", UUID: "vol-y"},
		}}
		devs := devicesOf(repeated)
		if ct := CoTenants(devs[0], devs); len(ct) != 0 {
			t.Errorf("per-controller repeats reported as co-tenants: %v", names(ct))
		}
	})

	t.Run("the method resolves and matches the pure form", func(t *testing.T) {
		r := &fakeDeviceResolver{devs: all}
		got, err := d.WithResolver(r).CoTenants(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(names(got), names(CoTenants(d, all))) {
			t.Errorf("Device.CoTenants = %v, want %v", names(got), names(CoTenants(d, all)))
		}
	})

	t.Run("the method sees a namespace attached since the snapshot", func(t *testing.T) {
		// d was resolved when it was alone on its subsystem, the very case
		// where reading the carried snapshot would wrongly allow a disconnect.
		alone := Subsystem{ID: sharedSubsys.ID, NQN: sharedSubsys.NQN,
			Namespaces: sharedSubsys.Namespaces[:1]}
		r := &fakeDeviceResolver{devs: devicesOf(alone)}
		stale := devicesOf(alone)[0].WithResolver(r)

		if got, _ := stale.HasCoTenants(context.Background()); got {
			t.Fatal("HasCoTenants = true while alone on the subsystem")
		}
		r.devs = all // a namespaced sibling volume showed up
		if got, err := stale.HasCoTenants(context.Background()); err != nil || !got {
			t.Errorf("HasCoTenants = %t, %v; want true, nil", got, err)
		}
	})

	t.Run("needs a resolver", func(t *testing.T) {
		if _, err := d.CoTenants(context.Background()); !errors.Is(err, errs.ErrUnsupported) {
			t.Errorf("CoTenants on an unbound device = %v, want errs.ErrUnsupported", err)
		}
	})
}

func TestHasCoTenants(t *testing.T) {
	solo := Subsystem{ID: "nvme-subsys1", NQN: "nqn.test:solo",
		Namespaces: []Namespace{{ID: 1, Name: "nvme1n1", UUID: "vol-x"}}}
	repeated := Subsystem{ID: "nvme-subsys2", NQN: "nqn.test:repeated", Namespaces: []Namespace{
		{ID: 1, Name: "nvme2c0n1", UUID: "vol-y"},
		{ID: 1, Name: "nvme2c1n1", UUID: "vol-y"},
	}}

	tests := []struct {
		name string
		sub  Subsystem
		want bool
	}{
		{"multi-namespace subsystem", sharedSubsys, true},
		{"single namespace", solo, false},
		{"same namespace per controller", repeated, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			all := devicesOf(tt.sub)
			if got := HasCoTenants(all[0], all); got != tt.want {
				t.Errorf("HasCoTenants = %t, want %t", got, tt.want)
			}
			if got := len(CoTenants(all[0], all)) > 0; got != tt.want {
				t.Errorf("CoTenants non-empty = %t, want %t (must agree with HasCoTenants)", got, tt.want)
			}
		})
	}

	t.Run("no subsystem nqn", func(t *testing.T) {
		d := dev("nvme0n1", "vol-a") // no subsystem snapshot at all
		if HasCoTenants(d, devicesOf(sharedSubsys)) {
			t.Error("HasCoTenants = true for a device with no subsystem NQN")
		}
		// And the method short-circuits rather than scanning.
		r := &countingResolver{fakeDeviceResolver{devs: devicesOf(sharedSubsys)}, 0}
		if got, err := d.WithResolver(r).HasCoTenants(context.Background()); got || err != nil {
			t.Errorf("HasCoTenants = %t, %v; want false, nil", got, err)
		}
		if r.calls != 0 {
			t.Errorf("resolver called %d times, want none", r.calls)
		}
	})
}

// countingResolver records how many lookups actually reached the resolver, so a
// test can assert that a short-circuit skipped the scan.
type countingResolver struct {
	fakeDeviceResolver
	calls int
}

func (c *countingResolver) ListWithSelector(ctx context.Context, sel DeviceSelector) ([]Device, error) {
	c.calls++
	return c.fakeDeviceResolver.ListWithSelector(ctx, sel)
}

// fakeDeviceResolver returns a fixed device list from List. Other methods are
// unused here.
type fakeDeviceResolver struct{ devs []Device }

func (f fakeDeviceResolver) List(context.Context) ([]Device, error) { return f.devs, nil }
func (f fakeDeviceResolver) ListWithSelector(_ context.Context, sel DeviceSelector) ([]Device, error) {
	return sel.Filter(f.devs), nil
}
func (f fakeDeviceResolver) ByUUID(context.Context, string) (Device, error) {
	return Device{}, nil
}
func (f fakeDeviceResolver) ByDevicePath(context.Context, string) (Device, error) {
	return Device{}, nil
}
func (f fakeDeviceResolver) ByNamespace(context.Context, string, NamespaceID) (Device, error) {
	return Device{}, nil
}

func TestDeviceSiblings_ReScans(t *testing.T) {
	const volA = "fee75e72-1291-4193-8357-3e228ced6c49"
	a1 := dev("nvme0n1", volA)
	a2 := dev("nvme1n1", volA)
	r := &fakeDeviceResolver{devs: []Device{a1, dev("nvme2n1", "other")}}

	// The device was resolved when it was the volume's only block device, so the
	// method must see the path that showed up since, not that snapshot.
	bound := a1.WithResolver(r)
	if got, err := bound.Siblings(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("Siblings = %v, %v; want none, nil", names(got), err)
	}
	r.devs = append(r.devs, a2)

	got, err := bound.Siblings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Namespace.Name != "nvme1n1" {
		t.Errorf("Siblings = %v, want [nvme1n1]", names(got))
	}
}
