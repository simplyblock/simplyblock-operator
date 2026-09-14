// The fleet-management suite: one Talos cluster, Open Cluster Management on it,
// and the fleet's add-on delivering the admission guard into it.
//
// What this proves that the unit and envtest suites cannot is the delivery
// chain. internal/guard's suites show that the rule decides correctly and that
// it installs into an API server somebody hands it. Here nobody hands it to
// anything: the template is rendered by OCM's addon-manager, carried in a
// ManifestWork, applied by a work agent, and only then does an API server have a
// policy to evaluate. Every step between the committed YAML and a refused write
// is somebody else's code.
//
// One cluster, managing itself. The guard runs wholly inside the member, so a
// second cluster would add a network and prove nothing more about it.

package fleet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
)

// The identities the guard decides between, as the suite impersonates them.
const (
	// allowedOperator is one of the three operator spellings the default
	// allowlist carries.
	allowedOperator = "system:serviceaccount:simplyblock:simplyblock-operator"

	// refusedUser is an ordinary administrator, which is what the guard exists
	// to refuse.
	refusedUser = "kubernetes-admin"
)

// refusedGroups is what an administrator's request carries. system:masters
// bypasses RBAC and not admission, which is the property the guard rests on.
var refusedGroups = []string{"system:masters"}

// step announces a phase and how long it took.
//
// Every phase here is minutes and none of them prints anything of its own, so
// without this a run that is working and a run that is wedged are the same
// silence. Under `go test -v` these stream as they happen, which is what says
// which phase to look at when one does wedge.
func step(t *testing.T, what string, run func()) {
	t.Helper()

	t.Logf("---> %s", what)
	start := time.Now()
	run()
	t.Logf("     done in %s", time.Since(start).Round(time.Second))
}

// clusterName keeps concurrent runs apart and stays inside two budgets: the
// QEMU monitor socket path that cluster.checkNameFits polices, and the sbi-
// prefix that `make clean-clusters` sweeps.
func clusterName() string {
	if n := os.Getenv("SB_CLUSTER_NAME"); n != "" {
		return n
	}
	return fmt.Sprintf("sbi-f%d", os.Getpid())
}

// TestFleetGuard boots the cluster once and runs every check against it, because
// booting Talos and registering it with Open Cluster Management is minutes and
// none of the checks below needs a cluster of its own.
func TestFleetGuard(t *testing.T) {
	if os.Getenv("SB_INTEGRATION") == "" {
		t.Skip("set SB_INTEGRATION=1 to run integration tests (boots QEMU virtual machines)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	c, err := cluster.Create(ctx, cluster.Config{
		Name: clusterName(),
		// Larger than the storage suite's node, because this one carries both
		// halves of Open Cluster Management at once: the hub's cluster-manager,
		// which runs three replicas, and the member's klusterlet beside it. At
		// the 2 GiB default the node has about 1.2 GiB allocatable, the hub takes
		// nearly all of it, and klusterlet-work-agent is left Pending. Nothing
		// then applies a ManifestWork, so the add-on delivers nothing and the
		// failure surfaces minutes later as a policy that never arrived.
		ControlplaneMemoryMB: 6144,
		// talosctl narrates the download, the boot, and the health checks, and
		// that is minutes of the run. Sent to the test log it says which of them
		// is taking the time.
		Logf: func(format string, args ...any) { t.Logf("  talos: "+format, args...) },
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Destroy(context.Background()); err != nil {
			t.Errorf("destroy cluster: %v", err)
		}
	})

	step(t, "waiting for the node", func() {
		if err := c.WaitNodesReady(ctx, 1, 8*time.Minute); err != nil {
			t.Fatalf("the node never became ready: %v", err)
		}
	})
	step(t, "installing Open Cluster Management and registering the cluster with itself", func() {
		installOCM(ctx, t, c)
	})
	step(t, "granting the work agent what the guard needs", func() {
		grantWorkAgent(ctx, t, c)
	})
	step(t, "installing the fleet's kinds and its add-on", func() {
		installFleet(ctx, t, c)
		grantProbeAccess(ctx, t, c)
	})
	step(t, "waiting for the add-on to deliver the guard", func() {
		installAddOn(ctx, t, c)
	})

	t.Run("the guard arrived whole", func(t *testing.T) {
		guardArrived(ctx, t, c)
	})

	t.Run("an ordinary administrator cannot write the storage group", func(t *testing.T) {
		out, err := applyAs(ctx, c, refusedUser, refusedGroups, operatorOps("refused"))
		if err == nil {
			t.Fatal("an administrator's write to the storage group was admitted")
		}
		if !strings.Contains(out, refusedUser) {
			t.Errorf("the refusal does not name the requester, so nobody can act on it: %s", out)
		}
	})

	t.Run("the operator's identity may write the storage group", func(t *testing.T) {
		if out, err := applyAs(ctx, c, allowedOperator, nil, operatorOps("allowed")); err != nil {
			t.Fatalf("the operator was refused, which stops the member reconciling: %v\n%s", err, out)
		}
	})

	t.Run("status stays writable", func(t *testing.T) {
		// Deliberate: the match excludes subresources, and the operator writes
		// status on its hottest path. A refusal here would be a regression in
		// the match rather than a tightening of it.
		out, err := asUser(ctx, c, refusedUser, refusedGroups,
			"-n", "default", "patch", "operatorops", "allowed",
			"--subresource", "status", "--type", "merge", "-p", `{"status":{"message":"probe"}}`)
		if err != nil {
			t.Fatalf("a status write was refused: %v\n%s", err, out)
		}
	})

	t.Run("an administrator cannot widen the allowlist", func(t *testing.T) {
		out, err := asUser(ctx, c, refusedUser, refusedGroups,
			"-n", addOnNamespace, "patch", "configmap", guardName,
			"--type", "merge", "-p", `{"data":{"allowedUsers":"kubernetes-admin"}}`)
		if err == nil {
			t.Fatal("the allowlist was editable by the identity it refuses")
		}
		if !strings.Contains(out, guardSelfName) {
			t.Errorf("the allowlist was refused by something other than the guard: %s", out)
		}
	})

	t.Run("a read is not admission's business", func(t *testing.T) {
		if out, err := asUser(ctx, c, refusedUser, refusedGroups,
			"-n", "default", "get", "operatorops"); err != nil {
			t.Fatalf("a read was refused, which admission cannot do: %v\n%s", err, out)
		}
	})

	t.Run("the add-on reports what it is", func(t *testing.T) {
		addOnHealth(ctx, t, c)
	})
}

// guardArrived checks every object the add-on template carries, by name.
//
// The allowlist's namespace is the one worth stating plainly: the binding
// resolves its parameter there and denies when it cannot, so an add-on that
// installs its policy without its allowlist refuses every write to the group in
// that member.
func guardArrived(ctx context.Context, t *testing.T, c *cluster.Cluster) {
	t.Helper()

	for _, object := range []struct {
		kind string
		name string
		args []string
	}{
		{"validatingadmissionpolicy", guardName, nil},
		{"validatingadmissionpolicybinding", guardName, nil},
		{"validatingadmissionpolicy", guardSelfName, nil},
		{"validatingadmissionpolicybinding", guardSelfName, nil},
		{"configmap", guardName, []string{"-n", addOnNamespace}},
	} {
		args := append(append([]string{}, object.args...), "get", object.kind, object.name)
		if out, err := c.Kubectl(ctx, args...); err != nil {
			t.Errorf("%s/%s did not reach the member: %v\n%s", object.kind, object.name, err, out)
		}
	}
}

// addOnHealth records what Open Cluster Management makes of an add-on whose
// template carries no workload.
//
// The ManifestWork's Applied condition is the assertion, because that is what
// says the payload reached the member. The add-on's own conditions are reported
// rather than asserted: its health probe watches Deployments and DaemonSets, and
// this template has neither, so what it concludes is a fact to learn from a run
// rather than one to pin before the first.
func addOnHealth(ctx context.Context, t *testing.T, c *cluster.Cluster) {
	t.Helper()

	waitFor(ctx, t, 5*time.Minute, "the ManifestWork never applied", func() error {
		out, err := c.Kubectl(ctx, "-n", memberName, "get", "manifestwork",
			"-o", `jsonpath={.items[*].status.conditions[?(@.type=="Applied")].status}`)
		if err != nil {
			return err
		}
		if !strings.Contains(out, "True") {
			return fmt.Errorf("applied is %q", strings.TrimSpace(out))
		}
		return nil
	})

	out, err := c.Kubectl(ctx, "-n", memberName, "get", "managedclusteraddon", addOnName,
		"-o", `jsonpath={range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}`)
	if err != nil {
		t.Errorf("reading the add-on's conditions: %v", err)
		return
	}
	t.Logf("add-on conditions with no workload to probe:\n%s", strings.TrimSpace(out))
}
