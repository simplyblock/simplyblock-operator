// What kind of device a network interface is, and what it is stacked on.
//
// Every software interface a host has sits under devices/virtual: a bridge, a
// bond, a VLAN, a VXLAN, a veth to a pod. The physical-or-not reading in
// netiface.go therefore collapses all of them into one answer. That answer is not enough
// to decide anything: a management address on a bond or on a tagged VLAN is an
// ordinary configuration, and the same address on a CNI bridge or a veth is the
// cluster's own plumbing. The two cases need opposite decisions and read
// identically without the kind.
//
// The kind comes from the device type the kernel publishes in the interface's
// uevent, which is the driver's own word for what it registered: `bond`,
// `bridge`, `vlan`, `vxlan`, `macvlan`, `ipvlan`, `team`. A driver that
// registers no device type gets no DEVTYPE line, and such an interface is
// reported as virtual with no kind rather than guessed at from its name. A host
// is free to call its veth pairs and its dummy interfaces anything at all.
//
// The stack comes from the lower_* and upper_* links every stacked interface
// exports, one per relation and in both directions. One reading answers both
// questions that matter: the members of a bond or a bridge, which is the only
// route from an aggregate to the slot, the memory node, and the link speed of
// the hardware under it, and what is stacked above a NIC, which is where the
// address lives on a host whose management network is tagged.

package inventory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/simplyblock/atlas/internal/sysfs"
)

// LinkKind is what sort of device an interface is, in the kernel's own
// spelling where the kernel has one.
type LinkKind string

const (
	// LinkPhysical is an interface backed by hardware.
	LinkPhysical LinkKind = "physical"

	// LinkLoopback is the loopback interface, decided by its ARPHRD type.
	LinkLoopback LinkKind = "loopback"

	// LinkBridge is a software bridge. It has members and is the one aggregate
	// whose address says more about who configured it than about the host: a
	// cluster's CNI bridge and a hypervisor host's guest bridge are the same
	// kind of device put to opposite purposes.
	LinkBridge LinkKind = "bridge"

	// LinkBond is a link aggregation. It has members, and it rather than any of
	// them is what an address is bound to.
	LinkBond LinkKind = "bond"

	// LinkTeam is the other link aggregation, teamd's.
	LinkTeam LinkKind = "team"

	// LinkVLAN is a tagged interface over a single parent, which is how most
	// fleets separate a management network from the data one.
	LinkVLAN LinkKind = "vlan"

	// LinkVXLAN is an overlay. It is bindable, and on a Kubernetes worker it is
	// usually the cluster's own pod network rather than anything a storage node
	// should serve over.
	LinkVXLAN LinkKind = "vxlan"

	// LinkMACVLAN and LinkIPVLAN are the two ways of giving one NIC several
	// identities.
	LinkMACVLAN LinkKind = "macvlan"
	LinkIPVLAN  LinkKind = "ipvlan"

	// LinkVirtual is an interface with no hardware behind it whose driver
	// registered no device type: a veth, a dummy, a tunnel. It is the honest
	// answer for a device the kernel does not name, and it is never bindable,
	// because nothing that reaches this value has been identified.
	LinkVirtual LinkKind = "virtual"
)

// Bindable reports whether an address held by an interface of this kind is one
// a service could bind and something outside the host could reach it on.
//
// Loopback is out because it reaches nothing, and LinkVirtual is out because it
// is the value for a device nothing identified: a veth to a pod and a dummy
// interface both land there, and admitting an unidentified device would be
// admitting those. A bridge is in, which is a change from refusing every
// bridge: a hypervisor host's management address lives on one, and whether a
// given bridge is that or a CNI bridge is answered by the addresses it holds
// rather than by its kind.
func (k LinkKind) Bindable() bool {
	switch k {
	case LinkPhysical, LinkBond, LinkTeam, LinkVLAN, LinkVXLAN, LinkMACVLAN, LinkIPVLAN, LinkBridge:
		return true
	default:
		return false
	}
}

// Aggregate reports whether an interface of this kind is built out of members,
// as opposed to derived from a single parent.
//
// The distinction is what a caller needs it for: an aggregate's members are
// several and interchangeable, so the hardware facts under it have to be
// combined, where a derived interface has exactly one parent and inherits its.
func (k LinkKind) Aggregate() bool {
	return k == LinkBridge || k == LinkBond || k == LinkTeam
}

// deviceTypeKey is the line the kernel writes the driver's device type on, in
// the generic uevent every device directory carries.
const deviceTypeKey = "DEVTYPE="

// kindOf is the kind of the interface at dir, given what the physical-or-not,
// loopback, and bridge readings already concluded.
//
// The device type is asked first and the directories a driver exports second,
// because the type is the driver's own word and the directories are evidence of
// it: a kernel that publishes no DEVTYPE still has bonding/ under every bond and
// bridge/ under every bridge, and a tree captured from one is the case the
// fallback exists for.
func kindOf(dir string, virtual, loopback, bridge bool) LinkKind {
	if loopback {
		return LinkLoopback
	}

	switch declared := LinkKind(deviceTypeAt(dir)); declared {
	case LinkBridge, LinkBond, LinkTeam, LinkVLAN, LinkVXLAN, LinkMACVLAN, LinkIPVLAN:
		return declared
	}
	if bridge {
		return LinkBridge
	}
	if isDir(filepath.Join(dir, "bonding")) {
		return LinkBond
	}

	// No device type and no driver directory, so nothing identified it. What
	// the tree says about hardware is then the whole answer.
	if virtual {
		return LinkVirtual
	}
	return LinkPhysical
}

// isDir reports whether the path is a directory, which is how a driver's own
// export is recognized.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// deviceTypeAt reads the DEVTYPE the kernel publishes for the device at dir, and
// the empty string for a device whose driver registered no type.
func deviceTypeAt(dir string) string {
	for _, line := range strings.Split(sysfs.String(dir, "uevent"), "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), deviceTypeKey); found {
			return value
		}
	}
	return ""
}

// stackAt is what the interface at dir is built on and what is built on it,
// each ascending by name so that two readings of one host are comparable.
//
// The links are read by name rather than followed, because the name is what
// identifies the other interface in the same reading and following the link
// would answer a question nobody asked.
func stackAt(dir string) (lower, upper []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}

	for _, entry := range entries {
		if name, found := strings.CutPrefix(entry.Name(), "lower_"); found {
			lower = append(lower, name)
			continue
		}
		if name, found := strings.CutPrefix(entry.Name(), "upper_"); found {
			upper = append(upper, name)
		}
	}
	slices.Sort(lower)
	slices.Sort(upper)
	return lower, upper
}
