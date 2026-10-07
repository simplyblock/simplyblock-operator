package netsetup

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestDefaultPlanAddressesTheGuestBehindTheBridge(t *testing.T) {
	p := DefaultPlan("eth0")

	if p.Uplink != "eth0" {
		t.Errorf("Uplink = %q, want eth0", p.Uplink)
	}
	// IFNAMSIZ is 16 including the terminator, so a longer name fails LinkAdd.
	for _, name := range []string{p.Bridge, p.Tap} {
		if name == "" || len(name) > 15 {
			t.Errorf("interface name %q is empty or longer than 15 bytes", name)
		}
	}
	if p.Bridge == p.Tap || p.Bridge == p.Uplink || p.Tap == p.Uplink {
		t.Errorf("interface names collide: bridge %q, tap %q, uplink %q", p.Bridge, p.Tap, p.Uplink)
	}

	if !p.Gateway.Addr().Is4() || !p.Gateway.Addr().IsLinkLocalUnicast() {
		t.Errorf("gateway %s is not IPv4 link-local", p.Gateway)
	}
	// The cloud metadata endpoint lives in 169.254.0.0/16 too, and a guest
	// subnet holding it would shadow it for the whole pod.
	if p.Gateway.Masked().Contains(netip.MustParseAddr("169.254.169.254")) {
		t.Errorf("guest subnet %s contains the metadata address", p.Gateway.Masked())
	}
	if !p.Gateway.Masked().Contains(p.Guest) || p.Guest == p.Gateway.Addr() {
		t.Errorf("guest %s is not another address in %s", p.Guest, p.Gateway.Masked())
	}
	if got, want := p.GuestAddress(), netip.PrefixFrom(p.Guest, p.Gateway.Bits()); got != want {
		t.Errorf("GuestAddress() = %s, want %s", got, want)
	}

	if len(p.GuestMAC) != 6 {
		t.Fatalf("GuestMAC %v is not 48 bits", p.GuestMAC)
	}
	// Locally administered, unicast: never a vendor's address, never multicast.
	if p.GuestMAC[0]&0x02 == 0 || p.GuestMAC[0]&0x01 != 0 {
		t.Errorf("GuestMAC %s is not a locally administered unicast address", p.GuestMAC)
	}
}

func TestNFSIsTheOnlyPortForwardedToTheGuest(t *testing.T) {
	p := DefaultPlan("eth0")
	var dnat []Rule
	for _, r := range p.Rules() {
		if slices.Contains(r.Spec, "DNAT") {
			dnat = append(dnat, r)
		}
	}
	want := Rule{
		Table: "nat",
		Chain: "PREROUTING",
		Spec: []string{
			"-i", "eth0", "-p", "tcp", "--dport", "2049",
			"-j", "DNAT", "--to-destination", p.Guest.String() + ":2049",
		},
	}
	if len(dnat) != 1 || !ruleEqual(dnat[0], want) {
		t.Errorf("DNAT rules = %v, want only %v", dnat, want)
	}
}

func TestGuestTrafficLeavesMasqueradedBehindThePodIP(t *testing.T) {
	p := DefaultPlan("ens5")
	want := Rule{
		Table: "nat",
		Chain: "POSTROUTING",
		Spec:  []string{"-s", p.Guest.String() + "/32", "-o", "ens5", "-j", "MASQUERADE"},
	}
	if !slices.ContainsFunc(p.Rules(), func(r Rule) bool { return ruleEqual(r, want) }) {
		t.Errorf("rules %v lack %v", p.Rules(), want)
	}
}

// The export allow-list names client addresses, so a client's source address
// has to reach the guest unchanged. Masquerading toward the bridge would
// replace every client with the bridge address and admit or refuse them all
// alike.
func TestInboundClientAddressesAreNotRewritten(t *testing.T) {
	p := DefaultPlan("eth0")
	for _, r := range p.Rules() {
		if !slices.Contains(r.Spec, "MASQUERADE") && !slices.Contains(r.Spec, "SNAT") {
			continue
		}
		spec := strings.Join(r.Spec, " ")
		if !strings.Contains(spec, "-s "+p.Guest.String()+"/32") || !strings.Contains(spec, "-o "+p.Uplink) {
			t.Errorf("source rewrite not confined to the guest's outbound traffic: %v", r)
		}
	}
}

func ruleEqual(a, b Rule) bool {
	return a.Table == b.Table && a.Chain == b.Chain && slices.Equal(a.Spec, b.Spec)
}
