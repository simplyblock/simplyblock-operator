package netsetup

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs/deferrers"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// inScratchNamespace runs fn in a new network namespace on a locked thread,
// so Apply's bridge, tap, sysctl, and rules never touch the host's. It needs
// root and iptables, and skips without them.
func inScratchNamespace(t *testing.T, fn func()) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a network namespace")
	}
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skip("needs iptables")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	host, err := netns.Get()
	if err != nil {
		t.Fatalf("current namespace: %v", err)
	}
	defer deferrers.Close(&host)
	scratch, err := netns.New()
	if err != nil {
		t.Fatalf("new namespace: %v", err)
	}
	defer deferrers.Close(&scratch)
	defer func() {
		if err := netns.Set(host); err != nil {
			t.Fatalf("returning to the host namespace: %v", err)
		}
	}()
	fn()
}

// The second Apply stands for a runner container restarting inside a pod that
// kept its network namespace: it has to find and reuse everything the first
// one built, and leave exactly one copy of each rule.
func TestApplyIsIdempotentAcrossRestarts(t *testing.T) {
	inScratchNamespace(t, func() {
		ctx := context.Background()
		p := DefaultPlan("lo")
		for i := range 2 {
			if err := Apply(ctx, p, ExecIPTables); err != nil {
				t.Fatalf("Apply #%d: %v", i+1, err)
			}
		}

		bridge, err := netlink.LinkByName(p.Bridge)
		if err != nil {
			t.Fatalf("bridge: %v", err)
		}
		addrs, err := netlink.AddrList(bridge, netlink.FAMILY_V4)
		if err != nil || len(addrs) != 1 || addrs[0].IPNet.String() != p.Gateway.String() {
			t.Errorf("bridge addresses = %v, %v, want only %s", addrs, err, p.Gateway)
		}
		tap, err := netlink.LinkByName(p.Tap)
		if err != nil {
			t.Fatalf("tap: %v", err)
		}
		if tap.Attrs().MasterIndex != bridge.Attrs().Index {
			t.Errorf("tap is not attached to the bridge")
		}

		for _, rule := range p.Rules() {
			out, err := exec.Command("iptables", "-w", "-t", rule.Table, "-S", rule.Chain).Output()
			if err != nil {
				t.Fatalf("listing %s/%s: %v", rule.Table, rule.Chain, err)
			}
			if n := countLines(string(out), "-A "+rule.Chain); n != 1 {
				t.Errorf("%s/%s holds %d rules after two runs, want 1:\n%s", rule.Table, rule.Chain, n, out)
			}
		}
	})
}

func countLines(s, prefix string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}
