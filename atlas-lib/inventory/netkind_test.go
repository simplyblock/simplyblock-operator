// What the interface reader reports for a stacked host: a bond over two NICs, a
// VLAN over the bond, a bridge over a third NIC, an overlay, a veth, and
// loopback.
//
// The tree is the one this reading exists for. Every one of those devices sits
// under devices/virtual, so the physical-or-not reading alone collapses them
// into one answer, and which of them a management address can be bound to
// differs for each.

package inventory

import (
	"reflect"
	"testing"
)

// stackedNetHost is a bonded host with a tagged management network, a bridge for
// guests, and a cluster's own plumbing beside it.
//
// bond0 carries no address of its own: the address is on the VLAN above it,
// which is how a tagged management network is configured and the case the
// physical-or-not reading cannot describe.
func stackedNetHost() fixture {
	const (
		eth0 = "devices/pci0000:00/0000:3b:00.0/net/eth0/"
		eth1 = "devices/pci0000:00/0000:3b:00.1/net/eth1/"
		eth2 = "devices/pci0000:00/0000:af:00.0/net/eth2/"
		bond = "devices/virtual/net/bond0/"
		vlan = "devices/virtual/net/bond0.100/"
		br   = "devices/virtual/net/br0/"
		vx   = "devices/virtual/net/vxlan.calico/"
		veth = "devices/virtual/net/veth7a1c/"
		lo   = "devices/virtual/net/lo/"
	)
	f := fixture{files: map[string]string{
		eth0 + "operstate": "up",
		eth0 + "type":      "1",
		eth0 + "speed":     "25000",
		eth0 + "uevent":    "INTERFACE=eth0\nIFINDEX=2",

		eth1 + "operstate": "up",
		eth1 + "type":      "1",
		eth1 + "speed":     "25000",
		eth1 + "uevent":    "INTERFACE=eth1\nIFINDEX=3",

		eth2 + "operstate": "up",
		eth2 + "type":      "1",
		eth2 + "speed":     "10000",
		eth2 + "uevent":    "INTERFACE=eth2\nIFINDEX=4",

		bond + "operstate":      "up",
		bond + "type":           "1",
		bond + "speed":          "50000",
		bond + "uevent":         "INTERFACE=bond0\nIFINDEX=5\nDEVTYPE=bond",
		bond + "bonding/slaves": "eth0 eth1",

		vlan + "operstate": "up",
		vlan + "type":      "1",
		vlan + "uevent":    "INTERFACE=bond0.100\nIFINDEX=6\nDEVTYPE=vlan",

		br + "operstate":      "up",
		br + "type":           "1",
		br + "uevent":         "INTERFACE=br0\nIFINDEX=7\nDEVTYPE=bridge",
		br + "bridge/root_id": "8000.0c42a15bc312",

		vx + "operstate": "unknown",
		vx + "type":      "1",
		vx + "uevent":    "INTERFACE=vxlan.calico\nIFINDEX=8\nDEVTYPE=vxlan",

		// A veth names no DEVTYPE, which is the kernel's answer for a device
		// whose driver registers no device type. It stays unnamed rather than
		// guessed at.
		veth + "operstate": "up",
		veth + "type":      "1",
		veth + "uevent":    "INTERFACE=veth7a1c\nIFINDEX=9",

		lo + "operstate": "unknown",
		lo + "type":      "772",
		lo + "uevent":    "INTERFACE=lo\nIFINDEX=1",

		"devices/pci0000:00/0000:3b:00.0/numa_node": "0",
		"devices/pci0000:00/0000:3b:00.1/numa_node": "0",
		"devices/pci0000:00/0000:af:00.0/numa_node": "1",
	}}
	f.links = map[string]string{
		"class/net/eth0":         "../../devices/pci0000:00/0000:3b:00.0/net/eth0",
		"class/net/eth1":         "../../devices/pci0000:00/0000:3b:00.1/net/eth1",
		"class/net/eth2":         "../../devices/pci0000:00/0000:af:00.0/net/eth2",
		"class/net/bond0":        "../../devices/virtual/net/bond0",
		"class/net/bond0.100":    "../../devices/virtual/net/bond0.100",
		"class/net/br0":          "../../devices/virtual/net/br0",
		"class/net/vxlan.calico": "../../devices/virtual/net/vxlan.calico",
		"class/net/veth7a1c":     "../../devices/virtual/net/veth7a1c",
		"class/net/lo":           "../../devices/virtual/net/lo",

		eth0 + "device": "../..",
		eth1 + "device": "../..",
		eth2 + "device": "../..",

		// The stack, as the kernel exports it: one link per relation, in both
		// directions.
		bond + "lower_eth0":      "../../../pci0000:00/0000:3b:00.0/net/eth0",
		bond + "lower_eth1":      "../../../pci0000:00/0000:3b:00.1/net/eth1",
		bond + "upper_bond0.100": "../bond0.100",
		eth0 + "upper_bond0":     "../../../../virtual/net/bond0",
		eth1 + "upper_bond0":     "../../../../virtual/net/bond0",
		vlan + "lower_bond0":     "../bond0",
		br + "lower_eth2":        "../../../pci0000:00/0000:af:00.0/net/eth2",
		eth2 + "upper_br0":       "../../../../virtual/net/br0",
	}
	return f
}

func TestReadInterfacesNamesTheKindOfEachDevice(t *testing.T) {
	root := stackedNetHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	want := map[string]LinkKind{
		"eth0":         LinkPhysical,
		"eth1":         LinkPhysical,
		"eth2":         LinkPhysical,
		"bond0":        LinkBond,
		"bond0.100":    LinkVLAN,
		"br0":          LinkBridge,
		"vxlan.calico": LinkVXLAN,
		"veth7a1c":     LinkVirtual,
		"lo":           LinkLoopback,
	}
	for name, kind := range want {
		if got := byName(t, ifaces, name).Kind; got != kind {
			t.Errorf("%s reads as kind %q, want %q", name, got, kind)
		}
	}
}

func TestReadInterfacesReportsTheMembersOfABondAndABridge(t *testing.T) {
	// A bond and a bridge carry no slot, no driver, and no memory node of their
	// own, so the members are the only route to the hardware underneath one.
	root := stackedNetHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	if got := byName(t, ifaces, "bond0").Lower; !reflect.DeepEqual(got, []string{"eth0", "eth1"}) {
		t.Errorf("bond0 reports members %v, want both NICs ascending", got)
	}
	if got := byName(t, ifaces, "br0").Lower; !reflect.DeepEqual(got, []string{"eth2"}) {
		t.Errorf("br0 reports members %v, want eth2", got)
	}
	if got := byName(t, ifaces, "eth0").Lower; len(got) != 0 {
		t.Errorf("a physical NIC reports members %v, and is built on nothing", got)
	}
}

func TestReadInterfacesReportsTheParentOfADerivedDevice(t *testing.T) {
	root := stackedNetHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	if got := byName(t, ifaces, "bond0.100").Lower; !reflect.DeepEqual(got, []string{"bond0"}) {
		t.Errorf("the VLAN reports %v as what it is built on, want bond0", got)
	}
}

func TestReadInterfacesReportsWhatIsStackedOnAnInterface(t *testing.T) {
	// The direction that matters for a NIC holding no address: what above it
	// might hold one.
	root := stackedNetHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	for _, name := range []string{"eth0", "eth1"} {
		if got := byName(t, ifaces, name).Upper; !reflect.DeepEqual(got, []string{"bond0"}) {
			t.Errorf("%s reports %v stacked on it, want bond0", name, got)
		}
	}
	if got := byName(t, ifaces, "bond0").Upper; !reflect.DeepEqual(got, []string{"bond0.100"}) {
		t.Errorf("bond0 reports %v stacked on it, want the VLAN", got)
	}
	if got := byName(t, ifaces, "eth2").Upper; !reflect.DeepEqual(got, []string{"br0"}) {
		t.Errorf("eth2 reports %v stacked on it, want br0", got)
	}
}

func TestReadInterfacesStillMarksEveryStackedDeviceVirtual(t *testing.T) {
	// The kind is an addition and not a replacement: a caller reading Virtual
	// keeps the answer it had.
	root := stackedNetHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	for _, name := range []string{"bond0", "bond0.100", "br0", "vxlan.calico", "veth7a1c", "lo"} {
		if !byName(t, ifaces, name).Virtual {
			t.Errorf("%s is under devices/virtual and was not marked virtual", name)
		}
	}
	if !byName(t, ifaces, "br0").Bridge {
		t.Error("br0 exports a bridge directory and was not marked a bridge")
	}
	for _, name := range []string{"bond0", "bond0.100"} {
		if byName(t, ifaces, name).Bridge {
			t.Errorf("%s was marked a bridge", name)
		}
	}
}

func TestLinkKindBindableSeparatesWhatAnAddressCanBeBoundTo(t *testing.T) {
	// The question the management-interface rule asks of a kind, kept beside the
	// kinds so that a kind added later has to answer it.
	bindable := map[LinkKind]bool{
		LinkPhysical: true,
		LinkBond:     true,
		LinkVLAN:     true,
		LinkVXLAN:    true,
		LinkMACVLAN:  true,
		LinkIPVLAN:   true,
		LinkTeam:     true,
		LinkBridge:   true,
		LinkLoopback: false,
		LinkVirtual:  false,
	}
	for kind, want := range bindable {
		if got := kind.Bindable(); got != want {
			t.Errorf("%s.Bindable() is %v, want %v", kind, got, want)
		}
	}
}

func TestLinkKindAggregatesAreTheOnesWithMembers(t *testing.T) {
	for _, kind := range []LinkKind{LinkBond, LinkBridge, LinkTeam} {
		if !kind.Aggregate() {
			t.Errorf("%s holds members and does not report itself as an aggregate", kind)
		}
	}
	for _, kind := range []LinkKind{LinkPhysical, LinkVLAN, LinkVXLAN, LinkLoopback, LinkVirtual} {
		if kind.Aggregate() {
			t.Errorf("%s reports itself as an aggregate", kind)
		}
	}
}

// kernelWithoutDeviceTypes is a bridge and a bond on a kernel that publishes no
// DEVTYPE for either, which is what a tree captured from an older kernel looks
// like. The directories the two drivers export are the fallback.
func kernelWithoutDeviceTypes() fixture {
	const (
		br   = "devices/virtual/net/br-mgmt/"
		bond = "devices/virtual/net/bond1/"
	)
	f := fixture{files: map[string]string{
		br + "operstate":        "up",
		br + "type":             "1",
		br + "bridge/stp_state": "0",

		bond + "operstate":      "up",
		bond + "type":           "1",
		bond + "bonding/slaves": "",
	}}
	f.links = map[string]string{
		"class/net/br-mgmt": "../../devices/virtual/net/br-mgmt",
		"class/net/bond1":   "../../devices/virtual/net/bond1",
	}
	return f
}

func TestReadInterfacesFallsBackToWhatTheDriverExports(t *testing.T) {
	root := kernelWithoutDeviceTypes().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	if got := byName(t, ifaces, "br-mgmt").Kind; got != LinkBridge {
		t.Errorf("a bridge with no DEVTYPE reads as %q, want %q", got, LinkBridge)
	}
	if got := byName(t, ifaces, "bond1").Kind; got != LinkBond {
		t.Errorf("a bond with no DEVTYPE reads as %q, want %q", got, LinkBond)
	}
}
