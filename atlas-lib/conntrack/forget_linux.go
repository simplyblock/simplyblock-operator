//go:build linux

// The Linux implementation of Forget: netlink's conntrack delete, filtered by
// the Selector's own matching so the rule is the one the tests check.

package conntrack

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// netlinkFilter adapts a Selector to netlink's filter interface.
type netlinkFilter struct{ sel Selector }

func (f netlinkFilter) MatchConntrackFlow(flow *netlink.ConntrackFlow) bool {
	return f.sel.Matches(tupleOf(flow))
}

// tupleOf reads the fields a Selector compares. Forward is the original
// direction, Reverse the reply direction, whose source is the backend a
// translated flow was sent to.
func tupleOf(flow *netlink.ConntrackFlow) Tuple {
	return Tuple{
		Protocol:    Protocol(flow.Forward.Protocol),
		OrigDst:     addrOf(flow.Forward.DstIP),
		OrigDstPort: flow.Forward.DstPort,
		ReplySource: addrOf(flow.Reverse.SrcIP),
	}
}

func addrOf(ip net.IP) netip.Addr {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}
	}
	return a.Unmap()
}

func forget(s Selector) (uint, error) {
	family := netlink.InetFamily(unix.AF_INET)
	if s.ReplySource.Is6() {
		family = netlink.InetFamily(unix.AF_INET6)
	}
	n, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, family, netlinkFilter{sel: s})
	if err != nil {
		return n, fmt.Errorf("conntrack: deleting flows to %s port %d: %w", s.ReplySource, s.DstPort, err)
	}
	return n, nil
}
