// Bringing Open Cluster Management up on one cluster, and registering that
// cluster as its own member.
//
// A hub may manage itself, and that is what this suite uses. The fleet's
// delivery chain runs entirely inside a member once a payload arrives, so one
// cluster exercises the whole of it: the add-on template is rendered into a
// ManifestWork by the hub's addon-manager, and the same cluster's work agent
// applies it. What one cluster cannot show is anything that depends on the two
// sides being apart, which is the dual-cluster suite's business.
//
// Everything here goes through the clusteradm binary rather than through
// manifests. The registration handshake is a token, a certificate signing
// request, and an approval, and reproducing that by hand would be a second
// implementation of something the project ships a tool for.

package fleet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
)

// memberName is what the cluster registers itself as. The name is OCM's
// convention for a hub that is also a member, and it becomes the namespace on
// the hub that this member's ManifestWork objects live in.
const memberName = "local-cluster"

// joinCommand carries the two values `clusteradm init` mints, out of the file it
// writes them to. Parsing a file it wrote for the purpose is steadier than
// parsing what it printed, which is prose around them.
var joinCommand = regexp.MustCompile(`--hub-token\s+(\S+)\s+--hub-apiserver\s+(\S+)`)

// clusteradmPath is the pinned binary in the repository's .bin, so a run uses
// the version the manifest names rather than whatever is on PATH.
func clusteradmPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), ".bin", "clusteradm")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("clusteradm is not installed: run `scripts/tools.sh install clusteradm`: %v", err)
	}
	return path
}

// clusteradm runs the tool against one cluster. The kubeconfig travels in the
// environment because that is the one selector every subcommand honors.
func clusteradm(ctx context.Context, t *testing.T, c *cluster.Cluster, timeout time.Duration, args ...string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, clusteradmPath(t), args...) //nolint:gosec // pinned binary, structured args
	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("clusteradm %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// installOCM initializes the hub, joins the cluster to itself, and returns once
// the member is registered and available.
func installOCM(ctx context.Context, t *testing.T, c *cluster.Cluster) {
	t.Helper()

	joinFile := filepath.Join(t.TempDir(), "join.sh")
	if _, err := clusteradm(ctx, t, c, 10*time.Minute,
		"init", "--wait", "--output-join-command-file", joinFile); err != nil {
		t.Fatalf("initializing the hub: %v", err)
	}

	raw, err := os.ReadFile(joinFile) //nolint:gosec // a path this test just created
	if err != nil {
		t.Fatalf("reading the join command clusteradm wrote: %v", err)
	}
	match := joinCommand.FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatalf("the join command names no token and API server, so the format changed: %q", string(raw))
	}
	token, apiserver := match[1], match[2]

	// The member is this same cluster, so the klusterlet reaches the hub through
	// the in-cluster service rather than through the address the host uses.
	if _, err := clusteradm(ctx, t, c, 10*time.Minute,
		"join",
		"--hub-token", token,
		"--hub-apiserver", apiserver,
		"--cluster-name", memberName,
		"--force-internal-endpoint-lookup",
		"--wait"); err != nil {
		t.Fatalf("joining the cluster to itself: %v", err)
	}

	if _, err := clusteradm(ctx, t, c, 10*time.Minute,
		"accept", "--clusters", memberName, "--wait"); err != nil {
		t.Fatalf("accepting the member: %v", err)
	}

	waitFor(ctx, t, 5*time.Minute, "the member never became available", func() error {
		out, err := c.Kubectl(ctx, "get", "managedcluster", memberName,
			"-o", `jsonpath={.status.conditions[?(@.type=="ManagedClusterConditionAvailable")].status}`)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "True" {
			return fmt.Errorf("availability is %q", strings.TrimSpace(out))
		}
		return nil
	})
}

// waitFor retries until the condition stops returning an error, which is how a
// suite waits for something no watch here is worth opening for.
//
// The optional diagnosis runs only when the wait gives up. A wait that times out
// says what did not happen, and the useful question is always why, so the
// failure carries the state that answers it.
func waitFor(
	ctx context.Context,
	t *testing.T,
	within time.Duration,
	what string,
	condition func() error,
	diagnose ...func() string,
) {
	t.Helper()

	fail := func(reason string, last error) {
		message := fmt.Sprintf("%s: %s (last result %v)", what, reason, last)
		for _, d := range diagnose {
			message += "\n" + d()
		}
		t.Fatal(message)
	}

	deadline := time.Now().Add(within)
	var last error
	for {
		if last = condition(); last == nil {
			return
		}
		if time.Now().After(deadline) {
			fail("gave up after "+within.String(), last)
		}
		select {
		case <-ctx.Done():
			fail(ctx.Err().Error(), last)
		case <-time.After(2 * time.Second):
		}
	}
}
