package nvme

import "testing"

func selDev(nqn, name string, nsid NamespaceID, uuid string) Device {
	return Device{
		Namespace: Namespace{ID: nsid, Name: name, DevicePath: "/dev/" + name, UUID: uuid},
		Subsystem: Subsystem{NQN: nqn},
	}
}

// selDevNGUID is selDev with the namespace NGUID set, which only the NGUID
// cases need. Real NGUIDs are 32 hex digits; sysfs reports them lower case.
func selDevNGUID(nqn, name string, nsid NamespaceID, uuid, nguid string) Device {
	d := selDev(nqn, name, nsid, uuid)
	d.Namespace.NGUID = nguid
	return d
}

func TestDeviceSelectorMatches(t *testing.T) {
	d := selDevNGUID(
		"nqn.test:vol", "nvme0n2", 2,
		"9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
		"516737547362616561385074624c4e6e",
	)

	cases := []struct {
		name string
		sel  DeviceSelector
		want bool
	}{
		{"zero selector matches anything", DeviceSelector{}, true},
		{"nqn", DeviceSelector{NQN: "nqn.test:vol"}, true},
		{"other nqn", DeviceSelector{NQN: "nqn.test:other"}, false},
		{"nsid", DeviceSelector{NSID: 2}, true},
		{"other nsid", DeviceSelector{NSID: 1}, false},
		{"uuid", DeviceSelector{UUID: "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d"}, true},
		{"uuid ignores case", DeviceSelector{UUID: "9B1DEB4D-3B7D-4BAD-9BDD-2B0D7B3DCB6D"}, true},
		{"other uuid", DeviceSelector{UUID: "00000000-0000-0000-0000-000000000000"}, false},
		{"nguid", DeviceSelector{NGUID: "516737547362616561385074624c4e6e"}, true},
		{"nguid ignores case", DeviceSelector{NGUID: "516737547362616561385074624C4E6E"}, true},
		{"other nguid", DeviceSelector{NGUID: "00000000000000000000000000000000"}, false},
		{"nguid and uuid are ANDed", DeviceSelector{
			NGUID: "516737547362616561385074624c4e6e",
			UUID:  "00000000-0000-0000-0000-000000000000",
		}, false},
		{"device path", DeviceSelector{DevicePath: "/dev/nvme0n2"}, true},
		{"other device path", DeviceSelector{DevicePath: "/dev/nvme0n1"}, false},
		{"fields are ANDed", DeviceSelector{NQN: "nqn.test:vol", NSID: 2}, true},
		{"one mismatched field rejects", DeviceSelector{NQN: "nqn.test:vol", NSID: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sel.Matches(d); got != tc.want {
				t.Errorf("%s.Matches(%s) = %v, want %v", tc.sel, d.Namespace.Name, got, tc.want)
			}
		})
	}
}

func TestDeviceSelectorFilter(t *testing.T) {
	devs := []Device{
		selDev("nqn.test:ns", "nvme0n1", 1, "aaaa"),
		selDev("nqn.test:ns", "nvme0n2", 2, "bbbb"),
		selDev("nqn.test:solo", "nvme1n1", 1, "cccc"),
	}

	// The multi-namespace case: NQN alone keeps both namespaces, in order.
	got := DeviceSelector{NQN: "nqn.test:ns"}.Filter(devs)
	if len(got) != 2 || got[0].Namespace.Name != "nvme0n1" || got[1].Namespace.Name != "nvme0n2" {
		t.Errorf("filter by nqn = %v, want [nvme0n1 nvme0n2]", devNames(got))
	}

	got = DeviceSelector{NQN: "nqn.test:ns", NSID: 2}.Filter(devs)
	if len(got) != 1 || got[0].Namespace.Name != "nvme0n2" {
		t.Errorf("filter by nqn+nsid = %v, want [nvme0n2]", devNames(got))
	}

	if got = (DeviceSelector{NQN: "nqn.absent"}).Filter(devs); len(got) != 0 {
		t.Errorf("filter with no match = %v, want empty", devNames(got))
	}
	if got = (DeviceSelector{}).Filter(devs); len(got) != len(devs) {
		t.Errorf("zero selector kept %d of %d devices", len(got), len(devs))
	}
	if got = (DeviceSelector{}).Filter(nil); got == nil || len(got) != 0 {
		t.Errorf("filter of nil = %v, want a non-nil empty slice", got)
	}
}

func TestDeviceSelectorString(t *testing.T) {
	if s := (DeviceSelector{}).String(); s != "any" {
		t.Errorf("zero selector = %q, want \"any\"", s)
	}
	sel := DeviceSelector{NQN: "nqn.x", NSID: 3, UUID: "u", NGUID: "n", DevicePath: "/dev/nvme0n3"}
	want := "nqn=nqn.x,nsid=3,uuid=u,nguid=n,device=/dev/nvme0n3"
	if s := sel.String(); s != want {
		t.Errorf("String() = %q, want %q", s, want)
	}
	if !(DeviceSelector{}).IsZero() || (DeviceSelector{NSID: 1}).IsZero() {
		t.Error("IsZero misreports")
	}
}

func devNames(devs []Device) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.Namespace.Name
	}
	return out
}
