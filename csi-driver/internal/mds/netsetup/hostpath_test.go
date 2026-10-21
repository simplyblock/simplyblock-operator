// Tests for the redirect of the guest's connections to its own node through
// the mds-proxy (design-pnfs-mds-vm.md §8.5): the rules the plan asks for, and
// rebuilding the chain they live in.

package netsetup

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// testDataInterface is a pod's attachment to the data network, as Multus names
// the first one.
const testDataInterface = "net1"

func sameRule(a, b Rule) bool {
	return a.Table == b.Table && a.Chain == b.Chain && slices.Equal(a.Spec, b.Spec)
}

func TestTheHostPathJumpIsOnlyInstalledWhenThePlanHasAProxy(t *testing.T) {
	jump := Rule{Table: "nat", Chain: "PREROUTING", Spec: []string{
		"-i", "mds-br0", "-s", "169.254.100.2/32", "-p", "tcp", "-j", HostPathChain,
	}}
	jumps := func(r Rule) bool { return slices.Contains(r.Spec, HostPathChain) }
	p := DefaultPlan("eth0")
	if slices.ContainsFunc(p.Rules(), jumps) {
		t.Errorf("a plan without a proxy jumps to %s", HostPathChain)
	}
	p.HostPath = true
	if !slices.ContainsFunc(p.Rules(), func(r Rule) bool { return sameRule(r, jump) }) {
		t.Errorf("Rules() = %v, want it to contain %v", p.Rules(), jump)
	}
}

// A pod with its own attachment to the data network sends only the
// connections to its own node through the proxy.
func TestHostPathRulesRedirectEachNodeAddressToTheForwarder(t *testing.T) {
	p := DefaultPlan("eth0")
	p.DataInterface = testDataInterface
	got := p.HostPathRules([]netip.Addr{netip.MustParseAddr("10.10.10.143"), netip.MustParseAddr("192.168.10.143")})
	redirect := func(addr string) Rule {
		return Rule{Table: "nat", Chain: HostPathChain, Spec: []string{
			"-d", addr, "-p", "tcp", "-j", "DNAT", "--to-destination", "169.254.100.1:7071",
		}}
	}
	want := []Rule{redirect("10.10.10.143/32"), redirect("192.168.10.143/32")}
	if !slices.EqualFunc(got, want, sameRule) {
		t.Errorf("HostPathRules = %v, want %v", got, want)
	}
	// The forwarder listens on the bridge's address, which the guest's
	// traffic arrives on.
	if got, want := p.ForwarderAddress(), netip.MustParseAddrPort("169.254.100.1:7071"); got != want {
		t.Errorf("ForwarderAddress = %s, want %s", got, want)
	}
}

// Without an attachment to the data network, every connection the guest opens
// goes through the proxy, whatever the node's addresses: the pod network may
// not route to the data network at all (OVN-Kubernetes sends it to the
// gateway), and the host always does. Link-local stays on the bridge.
func TestHostPathRulesSendEveryConnectionThroughTheProxyWithoutADataInterface(t *testing.T) {
	p := DefaultPlan("eth0")
	got := p.HostPathRules([]netip.Addr{netip.MustParseAddr("10.10.10.143")})
	want := []Rule{{Table: "nat", Chain: HostPathChain, Spec: []string{
		"!", "-d", "169.254.0.0/16", "-p", "tcp", "-j", "DNAT", "--to-destination", "169.254.100.1:7071",
	}}}
	if !slices.EqualFunc(got, want, sameRule) {
		t.Errorf("HostPathRules = %v, want %v", got, want)
	}
}

// chainIPTables answers -N, -F, and -A, and records every call.
type chainIPTables struct {
	exists bool
	newOut string
	newRC  int
	calls  []string
}

func (f *chainIPTables) run(_ context.Context, args ...string) (int, string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case slices.Contains(args, "-N"):
		if f.newRC != 0 {
			return f.newRC, f.newOut, nil
		}
		if f.exists {
			return 1, "iptables: Chain already exists.", nil
		}
		f.exists = true
	}
	return 0, "", nil
}

func TestReplaceChainCreatesAChainOnceAndRefillsIt(t *testing.T) {
	f := &chainIPTables{}
	p := DefaultPlan("eth0")
	p.DataInterface = testDataInterface
	rules := p.HostPathRules([]netip.Addr{netip.MustParseAddr("10.10.10.143")})
	for range 2 {
		if err := ReplaceChain(context.Background(), f.run, "nat", HostPathChain, rules); err != nil {
			t.Fatalf("ReplaceChain: %v", err)
		}
	}
	once := []string{
		"-w -t nat -N MDS-HOST-PATH",
		"-w -t nat -F MDS-HOST-PATH",
		"-w -t nat -A MDS-HOST-PATH -d 10.10.10.143/32 -p tcp -j DNAT --to-destination 169.254.100.1:7071",
	}
	want := append(slices.Clone(once), once...)
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestReplaceChainStopsWhenTheChainCannotBeCreated(t *testing.T) {
	f := &chainIPTables{newRC: 2, newOut: "iptables v1.8.10: can't initialize iptables table `nat'"}
	err := ReplaceChain(context.Background(), f.run, "nat", HostPathChain, nil)
	if err == nil {
		t.Fatal("ReplaceChain succeeded although -N failed")
	}
	if len(f.calls) != 1 {
		t.Errorf("ran %v after -N failed, want nothing", f.calls[1:])
	}
}
