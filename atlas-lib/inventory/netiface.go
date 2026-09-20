// Which network interfaces a host has, and how fast each link is.
//
// A storage cluster's throughput is bounded by the NIC its nodes serve NVMe-oF
// over, so the link speed is the number a deployment is planned against, and it
// is only readable per interface. Everything the kernel presents is reported,
// bridges and loopback included: which interface is a data NIC depends on the
// deployment's addressing rather than on anything readable here, so the reading
// hands a caller the evidence (physical or not, which slot, which driver, how
// fast) rather than making the choice for it.
//
// The reading follows the class/net symlink into the device tree, which is
// where all four of those facts live. Reading the attributes under class/net
// alone would answer none of them.

package inventory

import (
	"cmp"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"

	"github.com/simplyblock/atlas/internal/sysfs"
)

// LinkState is the kernel's operstate for an interface, in its own spelling.
type LinkState string

const (
	// LinkUp is a link that is carrying traffic.
	LinkUp LinkState = "up"

	// LinkDown is a link that is administratively or physically down.
	LinkDown LinkState = "down"

	// LinkUnknown is what the kernel reports for an interface whose driver does
	// not track carrier, loopback among them.
	LinkUnknown LinkState = "unknown"
)

// loopbackARPHRD is the ARPHRD type the kernel gives loopback, and the only
// reliable way to recognize it: the name lo is a convention rather than a rule.
const loopbackARPHRD = "772"

// Interface is one network interface as the kernel presents it.
type Interface struct {
	// Name is the kernel's name for the interface, such as eth0 or ens5f0.
	Name string

	// MACAddress is the link-layer address, lowercase and colon-separated.
	MACAddress string

	// MTU is the largest frame the interface carries.
	MTU int

	// OperState is the kernel's operstate.
	OperState LinkState

	// Carrier reports whether the driver sees a link partner.
	Carrier bool

	// SpeedMbps is the negotiated link speed in megabits per second, and is
	// zero where the kernel does not report one. A link that is down has no
	// speed, and neither does a virtual device, so zero means unknown rather
	// than slow.
	SpeedMbps int

	// Duplex is the kernel's duplex string: full, half, or unknown.
	Duplex string

	// Virtual reports whether the interface is backed by no hardware: a bridge,
	// a veth, a bond, a VLAN, or loopback.
	Virtual bool

	// Peered reports whether the interface names another device as its link,
	// which is read from iflink: an interface that points at itself stands
	// alone, and one whose iflink is another index is derived from or paired
	// with that device.
	//
	// It does not say which of those. A VLAN points at the parent it tags and a
	// veth points at the peer on the other side, and both read the same here.
	// What separates them is that a VLAN is identified — the kernel declares
	// DEVTYPE=vlan and exports a lower link — while a veth's peer is in another
	// namespace and nothing declares it at all. So a caller keeping pod links
	// out wants this together with the kind, not instead of it.
	Peered bool

	// Loopback reports whether this is the loopback interface, decided by its
	// ARPHRD type rather than by its name.
	Loopback bool

	// Driver is the name of the driver bound to the interface's device, and is
	// empty for a virtual one.
	Driver string

	// PCIAddress is the slot the interface's device sits in, in the
	// domain:bus:device.function form a deployment config names, and is empty
	// for a device on no PCI bus.
	PCIAddress string

	// NUMANode is the memory node the interface's device is attached to, or
	// NUMANodeUnknown. It is what decides whether a storage node pinned to one
	// socket reaches its NIC across the interconnect.
	NUMANode int

	// Kind is what sort of device the interface is, from the device type its
	// driver registered. It is what separates a bond or a tagged VLAN, which a
	// management address sits on in most fleets, from a veth or a CNI bridge,
	// which are the cluster's own plumbing: all of them are virtual, and only
	// the kind tells them apart.
	Kind LinkKind

	// Lower is what this interface is built on, ascending by name: the members
	// of a bond or a bridge, or the single parent of a VLAN or a macvlan. It is
	// empty for an interface built on nothing.
	//
	// It is the only route from an aggregate to the hardware under it. A bond
	// carries no slot, no driver, and no memory node of its own, so a caller
	// that has to know where a bonded management network physically lands reads
	// the members and looks them up in the same reading.
	Lower []string

	// Upper is what is built on this interface, ascending by name. It is the
	// direction that matters for a NIC holding no address of its own: on a host
	// whose management network is tagged, the address is on a VLAN above it.
	Upper []string

	// Bridge reports whether the interface is a software bridge.
	//
	// It is separate from Virtual, which a bridge also is, because the two
	// answer different questions. Virtual says the interface is backed by no
	// hardware; Bridge says it is carrying somebody else's traffic, which is
	// what makes a cluster's own bridge a poor choice for a management address
	// even where it holds one.
	Bridge bool

	// Addresses are the IP addresses assigned to the interface, as plain
	// addresses without a prefix length, in the order the host reports them.
	//
	// They do not come from sysfs, which does not carry them. They are read
	// through Config.InterfaceAddresses, and a caller that supplies none gets
	// none rather than an error: an interface reported without its addresses is
	// still worth reporting.
	Addresses []string
}

// AddressReader answers which IP addresses each interface holds, by interface
// name.
//
// It is a seam because sysfs does not carry addresses and the kernel's own
// answer is namespace-scoped: a process reading it reports the addresses of the
// network namespace it is in, so a pod without the host's network would report
// its own. The default reads this process's namespace, which is the host's when
// the caller runs with host networking, and a caller that cannot guarantee that
// supplies its own reader rather than being handed a confident wrong answer.
type AddressReader func() (map[string][]string, error)

// LocalAddresses reads the addresses of this process's network namespace.
func LocalAddresses() (map[string][]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list the network interfaces: %w", err)
	}

	out := make(map[string][]string, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			// One interface refusing its addresses is not a reason to lose the
			// rest, and an interface with none reported is reported with none.
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			out[iface.Name] = append(out[iface.Name], ip.String())
		}
	}
	return out, nil
}

// ReadInterfaces reads every network interface the host presents, ordered by
// name so that two readings of one host are comparable.
//
// A tree with no class/net has no interfaces, which is not an error: it is what
// a captured tree that did not include them looks like, and what a kernel
// without networking looks like.
func ReadInterfaces(cfg Config) ([]Interface, error) {
	base := cfg.sysfsPath("class/net")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", base, err)
	}

	// The addresses are read once for the whole host rather than per interface,
	// because the source answers for all of them at once. A reader that fails
	// costs the addresses and nothing else.
	addresses := map[string][]string{}
	if reader := cfg.addresses(); reader != nil {
		if read, err := reader(); err == nil {
			addresses = read
		}
	}

	ifaces := make([]Interface, 0, len(entries))
	for _, entry := range entries {
		iface := readInterface(filepath.Join(base, entry.Name()), entry.Name())
		iface.Addresses = addresses[entry.Name()]
		ifaces = append(ifaces, iface)
	}

	slices.SortFunc(ifaces, func(a, b Interface) int { return cmp.Compare(a.Name, b.Name) })
	return ifaces, nil
}

// readInterface reads one interface, whose attributes are all optional.
//
// Nothing here fails. Every attribute is missing on some interface somewhere —
// speed on a link that is down, duplex on a bridge, address on a device that
// has none — and an interface reported with the fields that were readable is
// worth more to a caller than a failure over the fields that were not.
func readInterface(dir, name string) Interface {
	iface := Interface{
		Name:       name,
		MACAddress: sysfs.String(dir, "address"),
		MTU:        sysfs.Int(0, dir, "mtu"),
		OperState:  LinkState(sysfs.String(dir, "operstate")),
		Carrier:    sysfs.Bool(dir, "carrier"),
		SpeedMbps:  linkSpeed(dir),
		Duplex:     sysfs.String(dir, "duplex"),
		Loopback:   sysfs.String(dir, "type") == loopbackARPHRD,
		NUMANode:   NUMANodeUnknown,
	}
	if index, link := sysfs.Int(0, dir, "ifindex"), sysfs.Int(0, dir, "iflink"); index != 0 && link != 0 {
		iface.Peered = index != link
	}
	if iface.OperState == "" {
		iface.OperState = LinkUnknown
	}

	// What the interface is stacked on is read before anything else, because it
	// is the one reading that answers for a device the rest of this function
	// returns early on: a bond has no slot and no driver, and its members are
	// where both of those are.
	iface.Lower, iface.Upper = stackAt(dir)

	// The class entry is a symlink into the device tree, and where that tree
	// says the interface sits is what decides whether it is backed by
	// hardware. A device under devices/virtual has none.
	resolved, err := filepath.EvalSymlinks(dir)
	if err == nil {
		iface.Virtual = sysfs.IsVirtual(resolved)
		// A bridge exports a bridge/ directory whatever it is named, which is
		// what makes this a reading rather than a guess at br0 and cni0 and
		// docker0.
		if entries, err := os.Stat(filepath.Join(dir, "bridge")); err == nil && entries.IsDir() {
			iface.Bridge = true
		}
	}
	iface.Kind = kindOf(dir, iface.Virtual, iface.Loopback, iface.Bridge)
	if err != nil || iface.Virtual {
		return iface
	}

	device, err := filepath.EvalSymlinks(filepath.Join(dir, "device"))
	if err != nil {
		// A physical interface with no device link is a driver that does not
		// export one, not a virtual device: what is known stays reported.
		return iface
	}
	iface.PCIAddress = sysfs.PCIAddressOf(device)
	iface.NUMANode = sysfs.Int(NUMANodeUnknown, device, "numa_node")
	if driver, err := os.Readlink(filepath.Join(device, "driver")); err == nil {
		iface.Driver = filepath.Base(driver)
	}
	return iface
}

// linkSpeed reads the negotiated speed, treating everything the kernel cannot
// answer as unknown.
//
// The attribute is refused with EINVAL for an interface with no carrier and
// reads as -1 for one whose driver has no answer, and both are the same fact: a
// speed nobody knows. Zero is that fact, and it is documented on the field so
// nothing reads it as a slow link.
func linkSpeed(dir string) int {
	speed := sysfs.Int(0, dir, "speed")
	if speed < 0 {
		return 0
	}
	return speed
}
