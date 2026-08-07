package nvme

import "testing"

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

	t.Run("HasSiblings agrees with Siblings", func(t *testing.T) {
		for _, d := range []Device{a1, a2, b1, dev("nvme9n1", volA), dev("nvme0n1", "")} {
			want := len(Siblings(d, all)) > 0
			if got := HasSiblings(d, all); got != want {
				t.Errorf("HasSiblings(%s) = %t, want %t", d.Namespace.Name, got, want)
			}
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
		// Without a multipath head a namespace appears once per controller;
		// those repeats are the same volume, not another tenant.
		repeated := Subsystem{ID: "nvme-subsys2", NQN: "nqn.test:repeated", Namespaces: []Namespace{
			{ID: 1, Name: "nvme2c0n1", UUID: "vol-y"},
			{ID: 1, Name: "nvme2c1n1", UUID: "vol-y"},
		}}
		devs := devicesOf(repeated)
		if ct := CoTenants(devs[0], devs); len(ct) != 0 {
			t.Errorf("per-controller repeats reported as co-tenants: %v", names(ct))
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
	})
}
