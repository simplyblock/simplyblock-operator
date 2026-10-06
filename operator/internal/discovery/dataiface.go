// Choosing the interfaces a storage node carries its data traffic on, and
// settling them against the management interface.
//
// A storage node without data interfaces serves its volumes on the management
// interface, which is a working default and the wrong one on a machine that has
// a storage network: the fast, jumbo-frame links sit idle while NVMe/TCP shares
// the management link. So the draft proposes them from what the probe read.
//
// The two planes are chosen together because they compete for the same links.
// Data is chosen first, since its requirements are the stricter ones, and the
// management ladder then runs over what is left. The one exception is the
// interface holding the address the cluster reaches the machine on, which stays
// pinned to management for the reasons ManagementOf gives.

package discovery

import (
	"cmp"
	"slices"
	"strings"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

const (
	// JumboMTU is the MTU from which a link runs jumbo frames, which ranks it
	// ahead of every link that does not.
	JumboMTU = 9000

	// MinDataSpeedMbps is the slowest link a data interface carries, resolved
	// through the stack for an aggregate.
	MinDataSpeedMbps = 10000
)

// Planes chooses the management interface and the data interfaces of one
// worker.
//
// Management keeps an interface of its own wherever the machine has two. The
// data interfaces are empty when none qualify, and when the only one that does
// is the management interface: the storage node then serves data on the
// management interface, which is what the control plane does when none are
// named.
func Planes(report nodeprobe.Report, nodeAddress string) (Management, []string) {
	index := interfacesByName(report)

	if pinned, found := holderOf(report, nodeAddress); found {
		mgmt := ManagementOf(report, nodeAddress)
		return mgmt, dataInterfaces(report, index, hardwareOf([]string{pinned.Name}, index))
	}

	data := dataInterfaces(report, index, nil)
	if mgmt := managementOf(report, nodeAddress, hardwareOf(data, index)); mgmt.Name != "" {
		return mgmt, data
	}

	// Data took every link management could use, so the ladder's own choice
	// among them goes back to management and data keeps the rest.
	mgmt := ManagementOf(report, nodeAddress)
	taken := hardwareOf([]string{mgmt.Name}, index)
	data = slices.DeleteFunc(data, func(name string) bool { return sharesHardware(name, index, taken) })
	if len(data) == 0 {
		data = nil
	}
	return mgmt, data
}

// holderOf is the interface holding the node's address, when it is one the
// management ladder accepts.
func holderOf(report nodeprobe.Report, nodeAddress string) (nodeprobe.Interface, bool) {
	if nodeAddress == "" {
		return nodeprobe.Interface{}, false
	}
	for _, iface := range report.Interfaces {
		if holdsAddress(iface, nodeAddress) && servesManagement(iface, true) {
			return iface, true
		}
	}
	return nodeprobe.Interface{}, false
}

// dataCandidate is an interface that qualifies for data, with the readings the
// ranking and the similarity test use.
type dataCandidate struct {
	name      string
	speedMbps int
	mtu       int
	driver    string
	aggregate bool
}

// dataInterfaces names the data interfaces of one report, ascending by name,
// passing over every interface that shares hardware with avoid.
//
// The candidates are ranked by jumbo frames first, then resolved speed, then
// aggregates before single NICs, then Mellanox before other drivers, then by
// name. The first is taken
// together with every other candidate that matches it in MTU, speed, driver,
// and aggregation, because the control plane spreads a node's listeners over
// all of its data interfaces and a slower or unlike one would set the pace.
func dataInterfaces(report nodeprobe.Report, index map[string]nodeprobe.Interface, avoid map[string]bool) []string {
	var candidates []dataCandidate
	for _, iface := range report.Interfaces {
		if !servesData(iface) || sharesHardware(iface.Name, index, avoid) {
			continue
		}
		speed := effectiveSpeed(iface.Name, index, map[string]bool{})
		if speed < MinDataSpeedMbps {
			continue
		}
		candidates = append(candidates, dataCandidate{
			name:      iface.Name,
			speedMbps: speed,
			mtu:       iface.MTU,
			driver:    driverOf(iface, index),
			aggregate: interfaceKind(iface).Aggregate(),
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	slices.SortFunc(candidates, func(a, b dataCandidate) int {
		return cmp.Or(
			compareTrueFirst(a.mtu >= JumboMTU, b.mtu >= JumboMTU),
			cmp.Compare(b.speedMbps, a.speedMbps),
			compareTrueFirst(a.aggregate, b.aggregate),
			compareTrueFirst(mellanox(a.driver), mellanox(b.driver)),
			cmp.Compare(a.name, b.name),
		)
	})

	best := candidates[0]
	var names []string
	for _, candidate := range candidates {
		if candidate.mtu == best.mtu && candidate.speedMbps == best.speedMbps &&
			candidate.driver == best.driver && candidate.aggregate == best.aggregate {
			names = append(names, candidate.name)
		}
	}
	slices.Sort(names)
	return names
}

// servesData reports whether an interface could carry a storage node's data
// traffic, before its speed is resolved.
//
// A physical NIC, a bond, or a team qualifies: a data interface is the link
// itself, and a VLAN or a bridge on top of one is somebody's network design for
// a reviewer to name rather than a reading. It has to be up, and it has to hold
// an IPv4 address: the control plane reads a data
// interface's address with `ip -j address show` and keeps the first `inet`
// entry only, then opens its listeners on that address and skips an interface
// that has none. An interface holding only IPv6 addresses is accepted by the
// control plane and serves nothing.
func servesData(iface nodeprobe.Interface) bool {
	switch interfaceKind(iface) {
	case inventory.LinkPhysical, inventory.LinkBond, inventory.LinkTeam:
	default:
		return false
	}
	if iface.State != "" && iface.State != "up" && iface.State != "unknown" {
		return false
	}
	return slices.ContainsFunc(iface.Addresses, func(address string) bool {
		return reachable(address) && ipOf(address).To4() != nil
	})
}

// driverOf is the driver behind an interface: its own for a NIC, and the one
// every member agrees on for an aggregate, or empty when they disagree.
func driverOf(iface nodeprobe.Interface, index map[string]nodeprobe.Interface) string {
	if iface.Driver != "" {
		return iface.Driver
	}
	driver := ""
	for _, member := range physicalUnder(iface.Lower, index, map[string]bool{}) {
		memberDriver := index[member].Driver
		if driver != "" && memberDriver != driver {
			return ""
		}
		driver = memberDriver
	}
	return driver
}

// mellanox reports whether a driver is one of NVIDIA Mellanox's (mlx4_core,
// mlx4_en, mlx5_core), whose cards are what a storage network is usually built
// on.
func mellanox(driver string) bool {
	return strings.HasPrefix(driver, "mlx")
}

// compareTrueFirst orders true before false.
func compareTrueFirst(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	default:
		return 1
	}
}

// hardwareOf is the set of interfaces the names given occupy: the names
// themselves and the physical interfaces under them.
func hardwareOf(names []string, index map[string]nodeprobe.Interface) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool)
	for _, name := range names {
		out[name] = true
		for _, member := range physicalUnder(index[name].Lower, index, map[string]bool{}) {
			out[member] = true
		}
	}
	return out
}

// sharesHardware reports whether an interface, or anything physical under it,
// is in the set given. A VLAN over a data bond is the same wire as the bond,
// and naming it for management would not separate the planes.
func sharesHardware(name string, index map[string]nodeprobe.Interface, set map[string]bool) bool {
	if len(set) == 0 {
		return false
	}
	if set[name] {
		return true
	}
	return slices.ContainsFunc(physicalUnder(index[name].Lower, index, map[string]bool{}),
		func(member string) bool { return set[member] })
}
