// What the interface reader reports for the four kinds of entry /sys/class/net
// holds: a physical NIC, a physical NIC whose link is down, a virtual device,
// and loopback.
//
// The fixture links class/net entries to a device tree rather than putting the
// attributes directly under class/net, because that is the one structural fact
// this reader depends on: whether an interface is physical, which driver binds
// it, and which PCI slot it sits in are all answers found by following that
// symlink, and a flat fixture would test none of it.

package inventory

import "testing"

// netHost carries one 25 GbE NIC that is up, one 10 GbE NIC whose link is down,
// a CNI bridge, and loopback.
func netHost() fixture {
	const (
		up   = "devices/pci0000:00/0000:3b:00.0/net/eth0/"
		down = "devices/pci0000:00/0000:3b:00.1/net/eth1/"
		br   = "devices/virtual/net/cni0/"
		lo   = "devices/virtual/net/lo/"
	)
	f := fixture{files: map[string]string{
		up + "address":   "0c:42:a1:5b:c3:10",
		up + "mtu":       "9000",
		up + "operstate": "up",
		up + "carrier":   "1",
		up + "speed":     "25000",
		up + "duplex":    "full",
		up + "type":      "1",

		down + "address":   "0c:42:a1:5b:c3:11",
		down + "mtu":       "1500",
		down + "operstate": "down",
		down + "carrier":   "0",
		down + "duplex":    "unknown",
		down + "type":      "1",

		br + "address":   "de:ad:be:ef:00:01",
		br + "mtu":       "1450",
		br + "operstate": "up",
		br + "carrier":   "1",
		br + "type":      "1",

		lo + "address":   "00:00:00:00:00:00",
		lo + "mtu":       "65536",
		lo + "operstate": "unknown",
		lo + "carrier":   "1",
		lo + "type":      "772",

		"devices/pci0000:00/0000:3b:00.0/numa_node": "0",
		"devices/pci0000:00/0000:3b:00.1/numa_node": "1",
	}}
	f.links = map[string]string{
		"class/net/eth0": "../../devices/pci0000:00/0000:3b:00.0/net/eth0",
		"class/net/eth1": "../../devices/pci0000:00/0000:3b:00.1/net/eth1",
		"class/net/cni0": "../../devices/virtual/net/cni0",
		"class/net/lo":   "../../devices/virtual/net/lo",

		up + "device":   "../..",
		down + "device": "../..",

		"devices/pci0000:00/0000:3b:00.0/driver": "../../../bus/pci/drivers/mlx5_core",
		"devices/pci0000:00/0000:3b:00.1/driver": "../../../bus/pci/drivers/mlx5_core",
	}
	f.dirs = []string{"bus/pci/drivers/mlx5_core"}
	return f
}

// byName finds one interface in a reading, failing the test when it is absent.
func byName(t *testing.T, ifaces []Interface, name string) Interface {
	t.Helper()
	for _, i := range ifaces {
		if i.Name == name {
			return i
		}
	}
	t.Fatalf("%s is missing from the reading %+v", name, ifaces)
	return Interface{}
}

func TestReadInterfacesReportsAPhysicalNICWhole(t *testing.T) {
	root := netHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	want := Interface{
		Name:       "eth0",
		MACAddress: "0c:42:a1:5b:c3:10",
		MTU:        9000,
		OperState:  LinkUp,
		Carrier:    true,
		SpeedMbps:  25000,
		Duplex:     "full",
		Driver:     "mlx5_core",
		PCIAddress: "0000:3b:00.0",
		NUMANode:   0,
	}
	if got := byName(t, ifaces, "eth0"); got != want {
		t.Errorf("read %+v, want %+v", got, want)
	}
}

func TestReadInterfacesReportsNoSpeedForALinkThatIsDown(t *testing.T) {
	// The kernel refuses to read speed for a NIC with no carrier, and the
	// attribute is missing rather than zero. Reporting zero is right, and
	// reporting a failure would lose the rest of the inventory over a NIC
	// nobody was going to use.
	root := netHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	eth1 := byName(t, ifaces, "eth1")
	if eth1.SpeedMbps != 0 {
		t.Errorf("read a speed of %d for a NIC that is down, want 0", eth1.SpeedMbps)
	}
	if eth1.OperState != LinkDown || eth1.Carrier {
		t.Errorf("read %q with carrier %v, want %q without", eth1.OperState, eth1.Carrier, LinkDown)
	}
	if eth1.Virtual {
		t.Error("a NIC that is down is still a physical NIC")
	}
	if eth1.NUMANode != 1 {
		t.Errorf("read NUMA node %d, want 1", eth1.NUMANode)
	}
}

func TestReadInterfacesMarksTheVirtualOnesAsVirtual(t *testing.T) {
	root := netHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	for _, name := range []string{"cni0", "lo"} {
		iface := byName(t, ifaces, name)
		if !iface.Virtual {
			t.Errorf("%s is under devices/virtual and was not marked virtual", name)
		}
		if iface.PCIAddress != "" || iface.Driver != "" {
			t.Errorf("%s reports driver %q at %q; a virtual device sits in no slot",
				name, iface.Driver, iface.PCIAddress)
		}
		if iface.NUMANode != NUMANodeUnknown {
			t.Errorf("%s reports NUMA node %d, want %d", name, iface.NUMANode, NUMANodeUnknown)
		}
	}

	if lo := byName(t, ifaces, "lo"); !lo.Loopback {
		t.Error("lo has ARPHRD type 772 and was not marked loopback")
	}
	if br := byName(t, ifaces, "cni0"); br.Loopback {
		t.Error("a bridge was marked loopback")
	}
}

func TestReadInterfacesIsOrderedByName(t *testing.T) {
	root := netHost().write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces: %v", err)
	}

	want := []string{"cni0", "eth0", "eth1", "lo"}
	if len(ifaces) != len(want) {
		t.Fatalf("read %d interfaces, want %d: %+v", len(ifaces), len(want), ifaces)
	}
	for i, name := range want {
		if ifaces[i].Name != name {
			t.Fatalf("interface %d is %s, want %s: the reading is ordered so that "+
				"two runs against one host agree", i, ifaces[i].Name, name)
		}
	}
}

func TestReadInterfacesReportsNoneRatherThanFailingWithoutTheClassDirectory(t *testing.T) {
	root := fixture{files: map[string]string{"meminfo": "MemTotal: 1024 kB"}}.write(t)

	ifaces, err := ReadInterfaces(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the interfaces of a tree without class/net: %v", err)
	}
	if len(ifaces) != 0 {
		t.Errorf("read %d interfaces from a tree with no class/net", len(ifaces))
	}
}
