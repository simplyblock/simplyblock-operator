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
	return []Rule{
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
}
