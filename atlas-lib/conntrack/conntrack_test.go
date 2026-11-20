// Tests for what a Selector matches. They need no conntrack table: the
// matching is plain Go, and the Linux file only adapts it to netlink.

package conntrack

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

var (
	deadPod   = netip.MustParseAddr("10.244.3.215")
	exportSvc = netip.MustParseAddr("10.102.100.19")
)

func nfsToDeadPod() Selector {
	return Selector{Protocol: TCP, OrigDst: exportSvc, DstPort: 2049, ReplySource: deadPod}
}

// The flow pinned in run pnfs-1791575321: a client's connection to an export's
// ClusterIP that kube-proxy translated to the replaced metadata server pod.
func TestAFlowTranslatedToTheOldAddressMatches(t *testing.T) {
	flow := Tuple{
		Protocol:    TCP,
		OrigDstPort: 2049,
		OrigDst:     netip.MustParseAddr("10.102.100.19"),
		ReplySource: deadPod,
	}
	if !nfsToDeadPod().Matches(flow) {
		t.Errorf("%+v does not match %+v", nfsToDeadPod(), flow)
	}
}

// Only that one address, port, and protocol: anything else on the host is
// another workload's connection.
func TestAnythingElseDoesNotMatch(t *testing.T) {
	base := Tuple{Protocol: TCP, OrigDst: exportSvc, OrigDstPort: 2049, ReplySource: deadPod}
	for name, flow := range map[string]Tuple{
		"another backend": {Protocol: TCP, OrigDst: exportSvc, OrigDstPort: 2049, ReplySource: netip.MustParseAddr("10.244.3.226")},
		"another port":    {Protocol: TCP, OrigDst: exportSvc, OrigDstPort: 443, ReplySource: deadPod},
		"UDP":             {Protocol: UDP, OrigDst: exportSvc, OrigDstPort: 2049, ReplySource: deadPod},
		"IPv6":            {Protocol: TCP, OrigDst: exportSvc, OrigDstPort: 2049, ReplySource: netip.MustParseAddr("::ffff:10.244.3.216")},
		// Review on #711: the dead pod's IP can be handed to another pod that
		// also serves 2049. Its flows go to another Service and stay.
		"another Service": {Protocol: TCP, OrigDst: netip.MustParseAddr("10.96.0.50"), OrigDstPort: 2049, ReplySource: deadPod},
	} {
		if nfsToDeadPod().Matches(flow) {
			t.Errorf("%s: %+v matched; only %+v should", name, flow, base)
		}
	}
}

// A selector that would match more than one address and port is refused: the
// point is to forget one dead backend's flows, not to flush a node.
func TestASelectorMissingAPartIsRefused(t *testing.T) {
	for name, sel := range map[string]Selector{
		"no protocol":    {OrigDst: exportSvc, DstPort: 2049, ReplySource: deadPod},
		"no port":        {Protocol: TCP, OrigDst: exportSvc, ReplySource: deadPod},
		"no address":     {Protocol: TCP, OrigDst: exportSvc, DstPort: 2049},
		"unspecified":    {Protocol: TCP, OrigDst: exportSvc, DstPort: 2049, ReplySource: netip.IPv4Unspecified()},
		"no destination": {Protocol: TCP, DstPort: 2049, ReplySource: deadPod},
		"unspecified destination": {
			Protocol: TCP, OrigDst: netip.IPv4Unspecified(), DstPort: 2049, ReplySource: deadPod,
		},
	} {
		if err := sel.Validate(); !errors.Is(err, ErrInvalidSelector) {
			t.Errorf("%s: Validate = %v, want ErrInvalidSelector", name, err)
		}
	}
	if err := nfsToDeadPod().Validate(); err != nil {
		t.Errorf("Validate(%+v) = %v, want nil", nfsToDeadPod(), err)
	}
}

func TestForgetRefusesAnInvalidSelector(t *testing.T) {
	_, err := Forget(Selector{Protocol: TCP})
	if !errors.Is(err, ErrInvalidSelector) && !errors.Is(err, errs.ErrUnsupported) {
		t.Errorf("Forget = %v, want ErrInvalidSelector (or ErrUnsupported off Linux)", err)
	}
}
