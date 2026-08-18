package suites

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
	"github.com/simplyblock/simplyblock-operator/test/integration/fabric"
)

// TestSmoke_NodeCanHostATarget is the harness's own test. Everything else in this
// tier assumes two things that nothing so far has checked: that the machine-config
// patch actually loads the NVMe-oF target modules, and that a privileged pod can
// reach the node's configfs. If either is false, every later suite fails somewhere
// far less legible than here.
//
// It deliberately involves no CSI driver and no control plane. A first run that
// fails should point at one thing.
func TestSmoke_NodeCanHostATarget(t *testing.T) {
	requireIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	c, err := cluster.Create(ctx, cluster.Config{Name: clusterName()})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	t.Cleanup(func() {
		// Its own context: the test's is cancelled by the time cleanup runs, and
		// a cluster left behind holds memory and a bridge for the whole run.
		if err := c.Destroy(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("destroy cluster: %v", err)
		}
	})

	if err := c.WaitNodesReady(ctx, 1, 5*time.Minute); err != nil {
		t.Fatalf("nodes never became ready: %v", err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("list nodes: %v (%v)", err, nodes)
	}
	t.Logf("cluster up with node %s", nodes[0])

	sh, err := fabric.NewShell(ctx, c, nodes[0])
	if err != nil {
		t.Fatalf("node shell: %v", err)
	}
	t.Cleanup(func() { _ = sh.Close(context.WithoutCancel(ctx)) })

	// The machine config asks for these at boot. Reading /proc/modules rather
	// than modprobing proves the patch worked, not merely that the modules exist.
	t.Run("machine config loaded the target modules", func(t *testing.T) {
		out, err := sh.LoadedModules(ctx)
		if err != nil {
			t.Fatalf("read /proc/modules: %v", err)
		}
		for _, mod := range []string{"nvmet", "nvmet_tcp"} {
			if !strings.Contains(out, mod+" ") {
				t.Errorf("module %s not loaded; the machine-config patch did not take effect", mod)
			}
		}
		// The initiator side is compiled into Talos' kernel rather than built as
		// modules, so it is absent from /proc/modules and present all the same.
		// Asserting on it here would fail for the wrong reason.
	})

	// Talos does not mount configfs, so the pod does. Without it a target cannot
	// be configured at all.
	var nvmet string
	t.Run("configfs is reachable and writable", func(t *testing.T) {
		var err error
		if nvmet, err = sh.EnsureConfigFS(ctx); err != nil {
			t.Fatalf("%v", err)
		}
		t.Logf("nvmet at %s", nvmet)
	})

	// attr_cntlid_min decides whether a same-NQN target pair is possible at all:
	// without it the host rejects the second controller as a duplicate cntlid,
	// and controller-not-contributing cannot be produced.
	t.Run("nvmet supports a same-NQN target pair", func(t *testing.T) {
		if nvmet == "" {
			t.Skip("configfs unavailable")
		}
		probe := nvmet + "/subsystems/nqn.integration.probe"
		out, err := sh.Run(ctx,
			"mkdir -p "+probe+" && ls "+probe+"/attr_cntlid_min; rmdir "+probe)
		if err != nil {
			t.Fatalf("probe subsystem: %v\n%s", err, out)
		}
		if !strings.Contains(out, "attr_cntlid_min") {
			t.Fatalf("nvmet lacks attr_cntlid_min; this kernel cannot host a same-NQN pair:\n%s", out)
		}
	})
}

// requireIntegration skips unless the suite was asked for. These tests boot
// virtual machines and take minutes, so they must not run as a side effect of
// `go test ./...` in a repository check.
func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_INTEGRATION") == "" {
		t.Skip("set SB_INTEGRATION=1 to run integration tests (boots QEMU virtual machines)")
	}
}

// clusterName keeps concurrent runs from colliding. talosctl names a cluster
// globally and its network with it, so two runs sharing a name fight over the
// same bridge and the second fails describing an interface it does not own.
// The pid suffix makes that impossible by accident; SB_CLUSTER_NAME pins it when
// a run has to be found again from outside, which is what `make
// integration-clean` needs.
func clusterName() string {
	if n := os.Getenv("SB_CLUSTER_NAME"); n != "" {
		return n
	}
	// Short on purpose. talosctl puts the name in the QEMU monitor socket path
	// twice, and that path has 104 bytes to live in; see cluster.checkNameFits.
	return fmt.Sprintf("sbi-%d", os.Getpid())
}
