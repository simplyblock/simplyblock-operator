package nvme

import (
	"context"
	"testing"
)

// dev builds a Device with the given namespace name/uuid; the sysfs path is
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
		got := names(a1.Siblings(all))
		if len(got) != 1 || got[0] != "nvme1n1" {
			t.Errorf("siblings of a1 = %v, want [nvme1n1]", got)
		}
	})

	t.Run("excludes the device itself", func(t *testing.T) {
		for _, d := range a1.Siblings(all) {
			if d.Namespace.SysfsPath == a1.Namespace.SysfsPath {
				t.Errorf("siblings must not include the device itself")
			}
		}
	})

	t.Run("single-head volume (native multipath) has no siblings", func(t *testing.T) {
		if got := b1.Siblings(all); len(got) != 0 {
			t.Errorf("siblings of b1 = %v, want none", names(got))
		}
	})

	t.Run("device need not be present in the list", func(t *testing.T) {
		a3 := dev("nvme9n1", volA) // another path to A, not in `all`
		got := names(a3.Siblings(all))
		if len(got) != 2 {
			t.Errorf("siblings of a3 = %v, want both a1 and a2", got)
		}
	})

	t.Run("no uuid yields nil", func(t *testing.T) {
		if got := dev("nvme0n1", "").Siblings(all); got != nil {
			t.Errorf("siblings for uuid-less device = %v, want nil", names(got))
		}
	})
}

func TestCoTenants(t *testing.T) {
	// A multi-namespace subsystem with three volumes.
	sub := Subsystem{
		ID:  "nvme-subsys0",
		NQN: "nqn.test:shared",
		Namespaces: []Namespace{
			{ID: 1, Name: "nvme0n1", UUID: "vol-a"},
			{ID: 2, Name: "nvme0n2", UUID: "vol-b"},
			{ID: 3, Name: "nvme0n3", UUID: "vol-c"},
		},
	}
	d := Device{Namespace: sub.Namespaces[0], Subsystem: sub} // nsid 1, vol-a

	got := d.CoTenants()
	if len(got) != 2 {
		t.Fatalf("CoTenants = %d, want 2", len(got))
	}
	for _, ct := range got {
		if ct.Namespace.ID == d.Namespace.ID {
			t.Error("CoTenants must exclude the device's own namespace")
		}
		if ct.Subsystem.NQN != sub.NQN {
			t.Errorf("co-tenant subsystem NQN = %q, want %q", ct.Subsystem.NQN, sub.NQN)
		}
	}

	// Single-namespace subsystem: no co-tenants.
	solo := Subsystem{ID: "nvme-subsys1", NQN: "nqn.test:solo", Namespaces: []Namespace{{ID: 1, UUID: "vol-x"}}}
	single := Device{Namespace: solo.Namespaces[0], Subsystem: solo}
	if ct := single.CoTenants(); len(ct) != 0 {
		t.Errorf("single-namespace CoTenants = %v, want none", names(ct))
	}
}

// fakeDeviceResolver returns a fixed device list from List; other methods are
// unused here.
type fakeDeviceResolver struct{ devs []Device }

func (f fakeDeviceResolver) List(context.Context) ([]Device, error) { return f.devs, nil }
func (f fakeDeviceResolver) ByUUID(context.Context, string) (Device, error) {
	return Device{}, nil
}
func (f fakeDeviceResolver) ByDevicePath(context.Context, string) (Device, error) {
	return Device{}, nil
}
func (f fakeDeviceResolver) ByNamespace(context.Context, string, NamespaceID) (Device, error) {
	return Device{}, nil
}

func TestSiblingsVia(t *testing.T) {
	const volA = "fee75e72-1291-4193-8357-3e228ced6c49"
	a1 := dev("nvme0n1", volA)
	a2 := dev("nvme1n1", volA)
	r := fakeDeviceResolver{devs: []Device{a1, a2, dev("nvme2n1", "other")}}

	got, err := SiblingsVia(context.Background(), r, a1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Namespace.Name != "nvme1n1" {
		t.Errorf("SiblingsVia = %v, want [nvme1n1]", names(got))
	}
}
