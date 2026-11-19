package volumemigration

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/nqn"
)

// HostNQNOfNode is the host NQN a migration Job pinned to node connects as:
// the node's simplyblock host NQN, derived from its UID exactly as the CSI node
// plugin derives it for NodeStage and for its reconnect loop.
//
// A path a Job connects becomes part of the volume's multipath device and, at
// cutover, its data path. Connected under any other identity it is a different
// host to the control plane: host fencing, allowed_hosts and DHCHAP are keyed on
// the host NQN, so such a path is outside all three, and a check comparing the
// hosts connected on the two sides of a cutover counts the one machine twice
// (bed bf28, 2026-11-19: the preconnect Jobs attached every target path as the
// kernel default nqn.2014-08.org.nvmexpress:uuid:<machine id>, while the CSI had
// staged the source as nqn.2014-08.io.simplyblock:uuid:<node uid>).
func HostNQNOfNode(ctx context.Context, c client.Reader, node string) (string, error) {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		return "", fmt.Errorf("read node %s for its host NQN: %w", node, err)
	}
	if n.UID == "" {
		return "", fmt.Errorf("node %s has no UID to derive a host NQN from", node)
	}
	return nqn.Host(string(n.UID)), nil
}

// WithHostNQN returns conns with every connection presenting hostNQN. An empty
// hostNQN leaves them as they are.
func WithHostNQN(conns []Connection, hostNQN string) []Connection {
	if hostNQN == "" {
		return conns
	}
	out := make([]Connection, len(conns))
	for i, c := range conns {
		c.HostNQN = hostNQN
		out[i] = c
	}
	return out
}
