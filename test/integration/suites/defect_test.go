package suites

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/nvmeof"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
	"github.com/simplyblock/simplyblock-operator/test/integration/fabric"
)

// The identity a simplyblock HA volume presents. The values are shaped like the
// real ones because the detection reads them: the model is the master lvol UUID,
// the serial is `ha`, and the same UUID appears in the NQN and in namespace 1.
const (
	volumeUUID = "4b7c6e02-1f3a-4d9e-9a51-0c2d8e6f7b13"
	testNQN    = "nqn.2023-04.io.simplyblock:integration:" + volumeUUID
	testSerial = "ha"
	testPort   = 4420
)

// TestDefect_ControllerNotContributing forces the defect that costs the most to
// reach by hand, and asserts that atlas names it from the kernel state alone.
//
// The shape is two nvmet targets on two nodes sharing one NQN, one exporting the
// namespace and one exporting nothing. A host that connects to both ends up with
// a single subsystem, two live controllers, and only one of them serving a path
// — which is precisely a controller that looks connected from every angle a
// connect checks while contributing neither redundancy nor I/O.
//
// Two nodes, and not two targets on one, because a node has one nvmet subsystem
// per NQN: the configfs directory is named after it. Sharing an NQN takes two
// kernels.
func TestDefect_ControllerNotContributing(t *testing.T) {
	requireIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	c, err := cluster.Create(ctx, cluster.Config{
		Name:    clusterNameFor("cnc"),
		Workers: 1,
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Destroy(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("destroy cluster: %v", err)
		}
	})

	if err := c.WaitNodesReady(ctx, 2, 5*time.Minute); err != nil {
		t.Fatalf("nodes never became ready: %v", err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil || len(nodes) < 2 {
		t.Fatalf("need two nodes, got %v (%v)", nodes, err)
	}
	ips := nodeIPs(ctx, t, c)
	serving, silent := nodes[0], nodes[1]
	t.Logf("serving target on %s (%s), silent target on %s (%s)",
		serving, ips[serving], silent, ips[silent])

	shServing, err := fabric.NewShell(ctx, c, serving)
	if err != nil {
		t.Fatalf("node shell on %s: %v", serving, err)
	}
	t.Cleanup(func() { _ = shServing.Close(context.WithoutCancel(ctx)) })

	shSilent, err := fabric.NewShell(ctx, c, silent)
	if err != nil {
		t.Fatalf("node shell on %s: %v", silent, err)
	}
	t.Cleanup(func() { _ = shSilent.Close(context.WithoutCancel(ctx)) })

	// Disjoint controller-ID ranges. Controller IDs are unique within a
	// subsystem rather than within a target, so two targets left on nvmet's
	// default range both hand out cntlid 1 and the host discards the second
	// controller as a duplicate — the topology collapses before the test starts.
	tServing, err := fabric.NewTarget(ctx, shServing, fabric.TargetSpec{
		NQN:       testNQN,
		Model:     volumeUUID,
		Serial:    testSerial,
		CntlIDMin: 1, CntlIDMax: 999,
		Addr: ips[serving], Port: testPort, PortID: 1,
		ANAState: "optimized",
	})
	if err != nil {
		t.Fatalf("create serving target: %v", err)
	}
	t.Cleanup(func() { _ = tServing.Close(context.WithoutCancel(ctx)) })

	if err := tServing.AddNamespace(ctx, 1, 64, volumeUUID); err != nil {
		t.Fatalf("add namespace: %v", err)
	}

	// No namespace here. An nvmet subsystem with none is a valid target that
	// accepts connects and answers Identify — the target side of a controller
	// that came up before its namespace did, or after its namespace went away.
	tSilent, err := fabric.NewTarget(ctx, shSilent, fabric.TargetSpec{
		NQN:       testNQN,
		Model:     volumeUUID,
		Serial:    testSerial,
		CntlIDMin: 1000, CntlIDMax: 1999,
		Addr: ips[silent], Port: testPort, PortID: 1,
		ANAState: "optimized",
	})
	if err != nil {
		t.Fatalf("create silent target: %v", err)
	}
	t.Cleanup(func() { _ = tSilent.Close(context.WithoutCancel(ctx)) })

	// The initiator is one of the two nodes; the host side is the node's kernel
	// either way, and the target it reaches over loopback is no different from
	// the one it reaches over the bridge.
	init, err := fabric.NewInitiator(ctx, shServing)
	if err != nil {
		t.Fatalf("prepare initiator: %v", err)
	}
	t.Cleanup(func() { _ = init.Disconnect(context.WithoutCancel(ctx), testNQN) })

	if err := init.Connect(ctx, testNQN, ips[serving], testPort); err != nil {
		t.Fatalf("connect to serving target: %v", err)
	}
	if err := init.Connect(ctx, testNQN, ips[silent], testPort); err != nil {
		t.Fatalf("connect to silent target: %v", err)
	}

	sel := nvme.DeviceSelector{NQN: testNQN, NSID: 1}

	var sub nvme.Subsystem
	sysroot := t.TempDir()
	snapshot := waitFor(ctx, t, 90*time.Second, func() (string, bool) {
		root := filepath.Join(sysroot, fmt.Sprint(time.Now().UnixNano()))
		snap, err := shServing.CaptureSysfs(ctx, root)
		if err != nil {
			return snap, false
		}
		s, err := nvme.NewSysfsSubsystemResolver(nvme.SysfsConfig{SysRoot: root}).
			ByNQN(ctx, testNQN)
		if err != nil {
			return snap, false
		}
		sub = s
		sysroot = root
		return snap, len(liveIDs(s)) == 2 && len(s.Namespaces) == 1
	})

	t.Run("the two targets present as one subsystem with two controllers", func(t *testing.T) {
		if got := len(liveIDs(sub)); got != 2 {
			t.Fatalf("want 2 live controllers under %s, got %d\n%s\n%s",
				testNQN, got, describe(sub), init.Describe(ctx))
		}
		if got := len(sub.Namespaces); got != 1 {
			t.Fatalf("want 1 namespace, got %d\n%s", got, describe(sub))
		}
		// Both controllers under one head is the whole premise: had the host
		// refused the second, there would be two subsystems and no defect to
		// find.
		t.Logf("subsystem %s: %s", sub.ID, describe(sub))
	})

	t.Run("the silent target's controller is reported as not contributing", func(t *testing.T) {
		cfg := nvme.SysfsConfig{SysRoot: sysroot}
		defects, err := nvmeof.Inspect(ctx,
			nvme.NewSysfsSubsystemResolver(cfg),
			nvme.NewSysfsDeviceResolver(cfg),
			sel, nil)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		if len(defects) != 1 {
			t.Fatalf("want exactly 1 defect, got %d: %+v\n%s", len(defects), defects, describe(sub))
		}

		d := defects[0]
		if d.Kind != nvmeof.DefectControllerNotContributing {
			t.Errorf("kind = %s, want %s (%s)", d.Kind, nvmeof.DefectControllerNotContributing, d.Detail)
		}
		if d.Scope != nvmeof.ScopeController {
			t.Errorf("scope = %v, want ScopeController: repairing this needs one controller torn down, not the subsystem", d.Scope)
		}
		if len(d.Controllers) != 1 {
			t.Fatalf("want 1 controller to tear down, got %d", len(d.Controllers))
		}

		// The named controller must be the silent target's. Naming the serving
		// one would be an actively harmful diagnosis: the repair would tear
		// down the only path to the namespace.
		if addr := d.Controllers[0].Address.TrAddr; addr != ips[silent] {
			t.Errorf("defect names the controller at %s, want the silent target at %s (serving target is %s)",
				addr, ips[silent], ips[serving])
		}
		if len(d.CoTenants) != 0 {
			t.Errorf("co-tenants = %v, want none: no other volume is attached", d.CoTenants)
		}
		t.Logf("defect: %s", d.Detail)
	})

	t.Run("the healthy pair reports nothing", func(t *testing.T) {
		// The same fabric, once the silent target exports the namespace too. The
		// expensive failure of this detection is not missing a defect but
		// inventing one, and a two-path volume is the state it must stay quiet
		// on.
		if err := tSilent.AddNamespace(ctx, 1, 64, volumeUUID); err != nil {
			t.Fatalf("add namespace to the formerly silent target: %v", err)
		}
		if err := init.Rescan(ctx, testNQN); err != nil {
			t.Logf("rescan: %v", err)
		}

		root := filepath.Join(t.TempDir(), "healthy")
		var last []nvmeof.Defect
		waitFor(ctx, t, 90*time.Second, func() (string, bool) {
			snap, err := shServing.CaptureSysfs(ctx, root)
			if err != nil {
				return snap, false
			}
			cfg := nvme.SysfsConfig{SysRoot: root}
			last, err = nvmeof.Inspect(ctx,
				nvme.NewSysfsSubsystemResolver(cfg),
				nvme.NewSysfsDeviceResolver(cfg),
				sel, nil)
			return snap, err == nil && len(last) == 0
		})
		if len(last) != 0 {
			t.Errorf("want no defects once both targets export the namespace, got %+v", last)
		}
	})

	saveSnapshot(t, "controller-not-contributing", snapshot)
}

// waitFor polls until check reports true, and returns the last snapshot it saw.
// It does not fail the test on timeout: the callers assert on the state
// afterward, where the failure message can say what was wrong with it.
func waitFor(ctx context.Context, t *testing.T, timeout time.Duration, check func() (string, bool)) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		snap, ok := check()
		if snap != "" {
			last = snap
		}
		if ok || time.Now().After(deadline) || ctx.Err() != nil {
			return last
		}
		time.Sleep(2 * time.Second)
	}
}

// liveIDs is the subsystem's live controllers.
func liveIDs(s nvme.Subsystem) []nvme.ControllerID {
	var out []nvme.ControllerID
	for _, c := range s.Controllers {
		if c.State == "live" {
			out = append(out, c.ID)
		}
	}
	return out
}

// describe renders a subsystem for a failure message: which controllers exist,
// where they point, and which of them serve each namespace.
func describe(s nvme.Subsystem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "subsystem %s (%s)\n", s.ID, s.NQN)
	for _, c := range s.Controllers {
		fmt.Fprintf(&b, "  controller %s cntlid=%d state=%s addr=%s\n",
			c.ID, c.CntlID, c.State, c.Address.TrAddr)
	}
	for _, ns := range s.Namespaces {
		fmt.Fprintf(&b, "  namespace %d uuid=%s paths=", ns.ID, ns.UUID)
		for i, p := range ns.Paths {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%s(%s)", p.Controller, p.ANAState)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// saveSnapshot writes the captured sysfs where it can become a unit-test
// fixture. The snapshot holds the fabric's identity, so it goes through
// hack/nvmet/capture-sysfs.sh sanitize before it is committed.
func saveSnapshot(t *testing.T, name, snapshot string) {
	t.Helper()
	dir := os.Getenv("SB_SNAPSHOT_DIR")
	if dir == "" || snapshot == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("snapshot dir: %v", err)
		return
	}
	path := filepath.Join(dir, name+".tsv")
	if err := os.WriteFile(path, []byte(snapshot), 0o600); err != nil {
		t.Logf("write snapshot: %v", err)
		return
	}
	t.Logf("sysfs snapshot written to %s (sanitize before committing)", path)
}

// nodeIPs maps node name to internal IP, which is the address a target
// advertises and an initiator dials.
func nodeIPs(ctx context.Context, t *testing.T, c *cluster.Cluster) map[string]string {
	t.Helper()
	out, err := c.Kubectl(ctx, "get", "nodes", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}`)
	if err != nil {
		t.Fatalf("node addresses: %v", err)
	}
	ips := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			ips[f[0]] = f[1]
		}
	}
	if len(ips) < 2 {
		t.Fatalf("want an internal IP for each node, got %v from:\n%s", ips, out)
	}
	return ips
}

// clusterNameFor keeps one spec's cluster from colliding with another's, since
// two clusters of one name cannot coexist on a host.
func clusterNameFor(suffix string) string {
	return clusterName() + "-" + suffix
}
