//go:build linux

// Reading the VLAN tags and VXLAN network identifiers over netlink.
//
// It is one RTM_GETLINK dump, the same request `ip -d link show` sends, and it
// has no file behind it, which is why it needs a build tag: there is no tree a
// developer's machine could be pointed at to produce the same answer.

package inventory

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// linkDumpAttempts bounds how often a dump the kernel interrupted is asked
// again. The kernel interrupts a dump when the link table changes while it is
// being read, which a pod starting on the node does, and a second dump almost
// always lands between two changes.
const linkDumpAttempts = 3

// LocalLinks reads the VLAN tag and VXLAN network identifier of every link in
// this process's network namespace, which is the host's when the caller runs
// with host networking. A link that is neither is left out of the map.
func LocalLinks() (map[string]LinkIdentity, error) {
	links, err := dumpLinks()
	if err != nil {
		return nil, err
	}

	out := map[string]LinkIdentity{}
	for _, link := range links {
		switch l := link.(type) {
		case *netlink.Vlan:
			out[l.Name] = LinkIdentity{VLAN: &VLANTag{ID: l.VlanId, Protocol: vlanProtocol(l.VlanProtocol)}}
		case *netlink.Vxlan:
			out[l.Name] = LinkIdentity{VXLAN: &VXLANOverlay{VNI: l.VxlanId}}
		}
	}
	return out, nil
}

// dumpLinks lists the links, asking again when the kernel reports that the
// table changed under the dump: the list it returns then may be missing links.
func dumpLinks() ([]netlink.Link, error) {
	var err error
	for range linkDumpAttempts {
		var links []netlink.Link
		if links, err = netlink.LinkList(); err == nil {
			return links, nil
		}
		if !errors.Is(err, netlink.ErrDumpInterrupted) {
			break
		}
	}
	return nil, fmt.Errorf("list the network links: %w", err)
}

// vlanProtocol spells the tag's ethertype the way iproute2 does. The library's
// own String gives `802.1q`, which is not how an administrator reads it.
func vlanProtocol(p netlink.VlanProtocol) string {
	switch p {
	case netlink.VLAN_PROTOCOL_8021Q:
		return "802.1Q"
	case netlink.VLAN_PROTOCOL_8021AD:
		return "802.1ad"
	default:
		return ""
	}
}
