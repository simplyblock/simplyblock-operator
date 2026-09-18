// Resolving an interface through the stack it sits in.
//
// An aggregate reports nothing about the hardware under it. A bond has no slot,
// no driver, no memory node, and usually no link speed of its own, because the
// kernel puts it under devices/virtual along with everything else that has no
// hardware. What it can carry, and where in the machine it lands, are facts
// about its members, and a VLAN over that bond inherits both from the bond.
//
// So the two questions the management rule asks of a candidate, how fast it is
// and which memory node it is on, are answered by walking down the stack the
// report carries, rather than by reading the interface and getting zero.

package discovery

import (
	"slices"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// interfaceKind is what sort of device an interface is, taking the probe's own
// answer where there is one.
//
// The fallback is for an interface whose kind the probe left empty, which is a
// fixture or a probe that could not read the device type rather than an older
// schema: a report from before the kind existed is refused by version. What was
// readable before the kind is then the whole answer, and it is read the way it
// was: virtual and unidentified is not something to bind to.
func interfaceKind(iface nodeprobe.Interface) inventory.LinkKind {
	if kind := inventory.LinkKind(iface.Kind); kind != "" {
		return kind
	}
	switch {
	case iface.Loopback:
		return inventory.LinkLoopback
	case iface.Bridge:
		return inventory.LinkBridge
	case iface.Virtual:
		return inventory.LinkVirtual
	default:
		return inventory.LinkPhysical
	}
}

// interfacesByName indexes a report's interfaces so that a stack walk can
// resolve a name to the interface it belongs to.
func interfacesByName(report nodeprobe.Report) map[string]nodeprobe.Interface {
	out := make(map[string]nodeprobe.Interface, len(report.Interfaces))
	for _, iface := range report.Interfaces {
		out[iface.Name] = iface
	}
	return out
}

// effectiveSpeed is what an interface can carry, in megabits per second.
//
// An interface that reports a speed is taken at its word. One that does not is
// resolved through what it is built on: an aggregate carries the sum of its
// members, because that is what aggregation is for, and a derived interface
// carries what its single parent carries, because it is the same wire with a
// tag on it.
//
// The walk is guarded against revisiting an interface. Nothing sysfs exports
// can produce a cycle, and a resolution that would hang on one is a resolution
// that trusts its input.
func effectiveSpeed(name string, index map[string]nodeprobe.Interface, seen map[string]bool) int {
	iface, known := index[name]
	if !known || seen[name] {
		return 0
	}
	seen[name] = true

	if iface.SpeedMbps > 0 {
		return iface.SpeedMbps
	}
	if interfaceKind(iface).Aggregate() {
		total := 0
		for _, member := range iface.Lower {
			total += effectiveSpeed(member, index, seen)
		}
		return total
	}
	for _, parent := range iface.Lower {
		if speed := effectiveSpeed(parent, index, seen); speed > 0 {
			return speed
		}
	}
	return 0
}

// physicalUnder is the physical interfaces reachable downward from the names
// given, ascending and without repeats.
//
// It is the only route from an aggregate to the hardware under it, and it is
// what lets a draft say which slots a bonded management network lands in.
func physicalUnder(names []string, index map[string]nodeprobe.Interface, seen map[string]bool) []string {
	var out []string
	for _, name := range names {
		iface, known := index[name]
		if !known || seen[name] {
			continue
		}
		seen[name] = true

		if interfaceKind(iface) == inventory.LinkPhysical {
			out = append(out, iface.Name)
			continue
		}
		out = append(out, physicalUnder(iface.Lower, index, seen)...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// numaNodeOf is the memory node a set of interfaces agrees on, and
// NUMANodeUnknown when they do not.
//
// Disagreement is a real answer and not a gap: a bond whose members are in two
// sockets has no affinity, and reporting one of the two would claim an affinity
// the interface does not have.
func numaNodeOf(names []string, index map[string]nodeprobe.Interface) int {
	node := inventory.NUMANodeUnknown
	for _, name := range names {
		iface, known := index[name]
		if !known {
			continue
		}
		if iface.NUMANode == inventory.NUMANodeUnknown {
			return inventory.NUMANodeUnknown
		}
		if node == inventory.NUMANodeUnknown {
			node = iface.NUMANode
			continue
		}
		if node != iface.NUMANode {
			return inventory.NUMANodeUnknown
		}
	}
	return node
}
