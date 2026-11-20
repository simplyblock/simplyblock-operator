//go:build linux

// Tests for the netlink adapter: that a kernel flow is read the way a Selector
// compares it. Building a flow needs no privilege. Deleting one does, and is not
// tested here.

package conntrack

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestTheAdapterReadsTheReplyDirectionsSource(t *testing.T) {
	flow := &netlink.ConntrackFlow{
		Forward: netlink.IPTuple{Protocol: 6, SrcIP: net.ParseIP("192.168.10.141"),
			DstIP: net.ParseIP("10.102.100.19"), SrcPort: 795, DstPort: 2049},
		Reverse: netlink.IPTuple{Protocol: 6, SrcIP: net.ParseIP("10.244.3.215"),
			DstIP: net.ParseIP("192.168.10.141"), SrcPort: 2049, DstPort: 795},
	}
	if !(netlinkFilter{sel: nfsToDeadPod()}).MatchConntrackFlow(flow) {
		t.Errorf("flow %+v not matched by %+v", tupleOf(flow), nfsToDeadPod())
	}
	flow.Reverse.SrcIP = net.ParseIP("10.244.3.226")
	if (netlinkFilter{sel: nfsToDeadPod()}).MatchConntrackFlow(flow) {
		t.Errorf("a flow to the replacement %+v matched", tupleOf(flow))
	}
}
