// One cluster for the package, and its nodes leased to the specs that need them.
//
// Booting Talos costs about a minute and a half, and this suite used to pay it
// once per spec: seven boots for a run whose actual testing takes two minutes.
// No spec needs a cluster of its own, though. What a spec needs is some number
// of nodes nobody else is using, and a node is much cheaper than a cluster —
// talosctl brings a cluster's machines up together, so a second node adds
// seconds to a boot where a second cluster adds the whole minute and a half.
//
// So the package boots one cluster, sized to the largest request any spec makes,
// and leases nodes out of it. A spec asking for one node is served by a cluster
// of two, and would be served by a cluster of three: the count a spec declares
// is a floor, not a shape to match.
//
// The cluster is booted by the first spec that asks for a node rather than by
// TestMain, so a run that selects nothing needing a cluster boots nothing.

package suites

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplyblock/simplyblock-operator/test/integration/cluster"
	"github.com/simplyblock/simplyblock-operator/test/integration/fabric"
)

// clusterNodes sizes the shared cluster. Go will not say which specs a run has
// selected before running them, so the largest requirement is declared here
// rather than derived from the specs themselves. leaseNodes fails loudly when a
// spec asks for more than this, which is the failure mode worth having: the
// alternative is a spec quietly served fewer nodes than it needs.
const clusterNodes = 2

// shared is the package's one cluster.
var shared = &nodePool{shells: map[string]*fabric.Shell{}}

// nodePool owns the cluster and decides which spec is on which node.
type nodePool struct {
	mu      sync.Mutex
	freed   *sync.Cond
	cluster *cluster.Cluster
	booted  bool
	bootErr error
	free    []string
	shells  map[string]*fabric.Shell
}

// ensure boots the cluster on the first call and returns it on every one. The
// error is remembered: once the boot has failed, every waiting spec should hear
// about that rather than queue up behind another attempt at it.
func (p *nodePool) ensure(ctx context.Context) (*cluster.Cluster, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.booted {
		return p.cluster, p.bootErr
	}
	p.booted = true
	p.freed = sync.NewCond(&p.mu)

	c, err := cluster.Create(ctx, cluster.Config{
		Name: clusterName(),
		// One controlplane comes for free; the rest are workers.
		Workers: clusterNodes - 1,
	})
	if err != nil {
		p.bootErr = fmt.Errorf("create cluster: %w", err)
		return nil, p.bootErr
	}
	p.cluster = c

	if err = c.WaitNodesReady(ctx, clusterNodes, 8*time.Minute); err != nil {
		p.bootErr = fmt.Errorf("nodes never became ready: %w", err)
		return nil, p.bootErr
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		p.bootErr = fmt.Errorf("list nodes: %w", err)
		return nil, p.bootErr
	}
	if len(nodes) < clusterNodes {
		p.bootErr = fmt.Errorf("the cluster came up with %d nodes and was asked for %d: %v",
			len(nodes), clusterNodes, nodes)
		return nil, p.bootErr
	}
	p.free = append(p.free, nodes...)
	return p.cluster, nil
}

// acquire takes n nodes out of the pool, waiting until that many are free. It
// takes all n or none, so two specs cannot each hold half of what they need.
func (p *nodePool) acquire(n int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.free) < n {
		p.freed.Wait()
	}
	taken := make([]string, n)
	copy(taken, p.free[:n])
	p.free = p.free[n:]
	return taken
}

// scrubScript returns a node to the state a lease expects to find it in.
//
// The expensive leftover is a controller whose target has gone away. Its block
// device stays present and every read on it blocks, so the next spec's `blkid`,
// `pvscan`, or `lvm` — every tool that enumerates devices rather than opening one
// by name — blocks with it and is eventually killed. That is invisible while each
// spec has a cluster to itself and fatal the moment two of them share a node.
//
// Nothing here may enumerate block devices, which rules out `vgs`, `pvscan`, and
// `blkid`: those are the commands the wedge hangs, so a scrub built on them hangs
// exactly when it is needed. `/proc/mounts` and `dmsetup ls` read kernel tables
// instead and answer whether or not a device behind them can be read, and once
// the controllers are gone there is no volume group left on the node to remove.
//
// Everything is guarded and best-effort: the pool runs it in whichever shells a
// node has, and the images differ in which tools they carry.
const scrubScript = `
set +e
for m in $(awk '$1 ~ "^/dev/(mapper/vol-|nvme)" {print $2}' /proc/mounts); do
  umount -f "$m" 2>/dev/null && echo "scrub: unmounted $m"
done
if command -v dmsetup >/dev/null 2>&1; then
  for dm in $(dmsetup ls 2>/dev/null | awk '$1 ~ "^vol-" {print $1}'); do
    dmsetup remove -f "$dm" >/dev/null 2>&1 && echo "scrub: removed device-mapper node $dm"
  done
fi
if command -v nvme >/dev/null 2>&1; then
  left=$(nvme list-subsys 2>/dev/null | grep -c "NQN=")
  if [ "${left:-0}" -gt 0 ]; then
    nvme disconnect-all >/dev/null 2>&1
    echo "scrub: disconnected ${left} leftover subsystem(s)"
  fi
fi
`

// scrub cleans one node through every shell the pool holds for it. What it had
// to clean is logged rather than swallowed: a spec that leaves something behind
// is a defect in that spec, and the next run should name it.
func (p *nodePool) scrub(ctx context.Context, t *testing.T, node string) {
	t.Helper()
	p.mu.Lock()
	shells := make([]*fabric.Shell, 0, len(p.shells))
	for key, sh := range p.shells {
		if strings.HasSuffix(key, "\x00"+node) {
			shells = append(shells, sh)
		}
	}
	p.mu.Unlock()

	for _, sh := range shells {
		out, err := sh.Run(ctx, scrubScript)
		if err != nil {
			t.Logf("scrub %s: %v\n%s", node, err, out)
			continue
		}
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			t.Logf("%s was not left clean by %s:\n%s", node, t.Name(), trimmed)
		}
	}
}

// release scrubs the nodes, puts them back, and wakes whoever is waiting.
func (p *nodePool) release(ctx context.Context, t *testing.T, nodes []string) {
	t.Helper()
	for _, node := range nodes {
		p.scrub(ctx, t, node)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = append(p.free, nodes...)
	p.freed.Broadcast()
}

// shell hands out a shell per node and image, kept for the cluster's lifetime.
// Starting one costs a pod launch and an image pull, and a spec that has leased
// a node is alone on it, so there is nothing to gain by tearing the pod down
// between specs and a pull to pay for putting it back.
func (p *nodePool) shell(ctx context.Context, node, image string) (*fabric.Shell, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := image + "\x00" + node
	if sh, ok := p.shells[key]; ok {
		return sh, nil
	}
	var opts []fabric.ShellOption
	if image != "" {
		opts = append(opts, fabric.WithImage(image))
	}
	sh, err := fabric.NewShell(ctx, p.cluster, node, opts...)
	if err != nil {
		return nil, err
	}
	p.shells[key] = sh
	return sh, nil
}

// shutdown takes the cluster down once the whole package is finished with it.
func (p *nodePool) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cluster == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The pods go with the cluster, so closing the shells is a courtesy rather
	// than a necessity; a failure to close one must not stop the destroy.
	for _, sh := range p.shells {
		_ = sh.Close(ctx)
	}
	if err := p.cluster.Destroy(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "destroy the shared cluster: %v\n", err)
	}
}

// TestMain destroys the shared cluster after the last spec has run. Nothing is
// created here: a cluster is booted by the first spec that leases a node, so a
// run that selects none boots none.
func TestMain(m *testing.M) {
	code := m.Run()
	shared.shutdown()
	os.Exit(code)
}

// leaseNodes gives a spec n nodes of the shared cluster, exclusively, for as
// long as it runs. The count is what the spec cannot do without: one node for
// anything staged on a single host, two for anything about a second one.
func leaseNodes(ctx context.Context, t *testing.T, n int) (*cluster.Cluster, []string) {
	t.Helper()
	if n > clusterNodes {
		t.Fatalf("this spec asks for %d nodes and the shared cluster carries %d; "+
			"raise clusterNodes", n, clusterNodes)
	}
	c, err := shared.ensure(ctx)
	if err != nil {
		t.Fatalf("bring up the shared cluster: %v", err)
	}
	nodes := shared.acquire(n)
	t.Cleanup(func() {
		// Its own context and its own deadline: the spec's is canceled by the time
		// cleanup runs, and a node put back dirty is the next spec's failure.
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		shared.release(clean, t, nodes)
	})
	return c, nodes
}

// leaseShell is the shell on node running image, started once per pair and
// shared by every spec that lands there. An empty image asks for the default.
func leaseShell(ctx context.Context, t *testing.T, node, image string) *fabric.Shell {
	t.Helper()
	sh, err := shared.shell(ctx, node, image)
	if err != nil {
		t.Fatalf("start a shell on %s: %v", node, err)
	}
	return sh
}
