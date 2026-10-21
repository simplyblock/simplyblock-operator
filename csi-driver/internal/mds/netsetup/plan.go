// The MDS pod's network: a bridge and a tap for the guest, the guest's
// link-local address, and the NAT rules that make the guest reachable at the
// pod IP.
//
// The guest's only path to the world is the pod's. Inbound NFS (TCP 2049) is
// DNATed to the guest, which keeps the client's source address for the export
// allow-list, and the guest's outbound traffic (NVMe/TCP to the storage
// cluster) is masqueraded behind the pod IP. Nothing else is forwarded, so the
// guest agent's port is reachable only from the runner across the bridge. The
// addresses are fixed rather than allocated, because the bridge is private to
// the pod and nothing outside it ever sees them. The technique follows
// neonvm-runner's net.go, without dnsmasq: the guest's address arrives on the
// kernel command line.
//
// The plan is pure so the rules can be tested without a network namespace.
// Applying it is apply_linux.go.

package netsetup

import (
	"net"
	"net/netip"
	"strconv"
)

// NFSPort is the only port forwarded to the guest. NFSv4.1 needs no
// portmapper, and mountd and statd serve only earlier protocol versions.
const NFSPort = 2049

// AgentPort is where the guest agent serves the export service and its health.
// It is not forwarded: only the runner reaches it, across the bridge.
const AgentPort = 7070

const (
	bridgeName = "mds-br0"
	tapName    = "mds-tap0"
)

// The guest subnet: a /30 holding the bridge and the guest, in 169.254.0.0/16
// so it cannot collide with a pod, service, or node CIDR, and away from
// 169.254.169.254, the cloud metadata endpoint.
var (
	gatewayAddress = netip.MustParsePrefix("169.254.100.1/30")
	guestAddress   = netip.MustParseAddr("169.254.100.2")
)

// guestMAC is locally administered (0x02) and unicast. The remaining bytes
// spell "sb" and the guest's last octet, for whoever reads a neighbor table.
var guestMAC = net.HardwareAddr{0x02, 0x73, 0x62, 0x00, 0x00, 0x02}

// Rule is one iptables rule: a table, a chain, and the rule specification
// without the command (-A, -C, -D).
type Rule struct {
	Table string
	Chain string
	Spec  []string
}

// Plan is the pod's guest network.
type Plan struct {
	// Uplink is the pod's own interface, the one carrying the pod IP.
	Uplink string
	Bridge string
	Tap    string

	// Gateway is the bridge's address and the guest's default route, with the
	// prefix both share.
	Gateway netip.Prefix
	Guest   netip.Addr
	// GuestMAC is fixed: the bridge holds one guest, so there is nothing for
	// it to collide with, and a stable MAC keeps ARP state valid across a
	// guest restart within the same pod.
	GuestMAC net.HardwareAddr

	// DataInterface is the pod's attachment to the storage data network, or
	// empty for a pod on the pod network alone. Its subnet is a connected
	// route in the pod, so the guest's traffic to the storage nodes leaves
	// through it rather than the uplink.
	DataInterface string

	// HostPath sends the guest's connections to its own node's addresses
	// through the mds-proxy: the guest's TCP on the bridge enters
	// HostPathChain, whose redirects the runner keeps current with
	// HostPathRules.
	HostPath bool
}

// DefaultPlan returns the plan for a pod whose own interface is uplink.
func DefaultPlan(uplink string) Plan {
	return Plan{
		Uplink:   uplink,
		Bridge:   bridgeName,
		Tap:      tapName,
		Gateway:  gatewayAddress,
		Guest:    guestAddress,
		GuestMAC: guestMAC,
	}
}

// GuestAddress returns the guest's address with the bridge's prefix, in the
// shape the guest's kernel command line takes.
func (p Plan) GuestAddress() netip.Prefix {
	return netip.PrefixFrom(p.Guest, p.Gateway.Bits())
}

// Rules returns the iptables rules the plan needs, in the order they are
// applied.
func (p Plan) Rules() []Rule {
	nfs := strconv.Itoa(NFSPort)
	rules := []Rule{
		// Clients reach nfsd at the pod IP. DNAT rewrites only the
		// destination, so the guest sees the client's own address.
		{Table: "nat", Chain: "PREROUTING", Spec: []string{
			"-i", p.Uplink, "-p", "tcp", "--dport", nfs,
			"-j", "DNAT", "--to-destination", p.Guest.String() + ":" + nfs,
		}},
		// The guest's own connections, NVMe/TCP to the storage cluster, leave
		// as the pod. Confined to the guest's source address and the uplink,
		// so replies to DNATed clients are never rewritten.
		{Table: "nat", Chain: "POSTROUTING", Spec: []string{
			"-s", p.Guest.String() + "/32", "-o", p.Uplink, "-j", "MASQUERADE",
		}},
	}
	if p.HostPath {
		// The guest's connections enter the chain of redirects to the
		// forwarder before routing, which would otherwise send a connection
		// to the node's own address out through the CNI and into a listener
		// that refuses it.
		rules = append(rules, Rule{Table: "nat", Chain: "PREROUTING", Spec: []string{
			"-i", p.Bridge, "-s", p.Guest.String() + "/32", "-p", "tcp", "-j", HostPathChain,
		}})
	}
	if p.DataInterface != "" {
		// The same for connections to the data network, which leave as the
		// pod's address there and so arrive at the storage nodes on their data
		// interfaces, where their listeners are bound.
		rules = append(rules, Rule{Table: "nat", Chain: "POSTROUTING", Spec: []string{
			"-s", p.Guest.String() + "/32", "-o", p.DataInterface, "-j", "MASQUERADE",
		}})
	}
	return rules
}

// linkLocal is the guest's own subnet's range, which never leaves the bridge.
var linkLocal = netip.MustParsePrefix("169.254.0.0/16")

// HostPathChain holds the redirects to the forwarder: one per address of the
// pod's node, rebuilt when the node's addresses change, or one for everything
// when the pod has no attachment to the data network.
const HostPathChain = "MDS-HOST-PATH"

// ForwarderPort is where the runner's forwarder takes the redirected
// connections, on the bridge's address.
const ForwarderPort = 7071

// ForwarderAddress is where the forwarder listens: the bridge's address,
// which the guest's traffic arrives on.
func (p Plan) ForwarderAddress() netip.AddrPort {
	return netip.AddrPortFrom(p.Gateway.Addr(), ForwarderPort)
}

// HostPathRules are HostPathChain's rules for a node holding addrs: each of
// the guest's TCP connections to one of them goes to the forwarder. DNAT
// rather than REDIRECT, which needs a kernel module the pod's other rules do
// not, and the forwarder reads the original destination the same way after
// either.
//
// A pod without an attachment to the data network sends every TCP connection
// of the guest's there, except to link-local addresses, which stay on the
// bridge: whether the pod network routes to the storage data network depends on
// the CNI (OVN-Kubernetes sends it to the gateway, where it is lost), and the
// host always does.
func (p Plan) HostPathRules(addrs []netip.Addr) []Rule {
	if p.DataInterface == "" {
		return []Rule{{Table: "nat", Chain: HostPathChain, Spec: []string{
			"!", "-d", linkLocal.String(), "-p", "tcp", "-j", "DNAT", "--to-destination", p.ForwarderAddress().String(),
		}}}
	}
	rules := make([]Rule, 0, len(addrs))
	for _, a := range addrs {
		rules = append(rules, Rule{Table: "nat", Chain: HostPathChain, Spec: []string{
			"-d", a.String() + "/32", "-p", "tcp", "-j", "DNAT", "--to-destination", p.ForwarderAddress().String(),
		}})
	}
	return rules
}

// noPayloadMax is the longest packet that carries no TCP payload: a 20-byte IP
// header and a 60-byte TCP header with every option.
const noPayloadMax = 80

// DrainRule stops requests from reaching the guest's nfsd while the guest
// shuts down. It drops every forwarded packet to the guest's NFS port that
// carries data. A client's bare ACKs still pass, so a reply larger than one
// TCP window, already being sent, still completes.
func (p Plan) DrainRule() Rule {
	return Rule{Table: "filter", Chain: "FORWARD", Spec: []string{
		"-d", p.Guest.String() + "/32", "-p", "tcp", "--dport", strconv.Itoa(NFSPort),
		"-m", "length", "!", "--length", "0:" + strconv.Itoa(noPayloadMax), "-j", "DROP",
	}}
}
