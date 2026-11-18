// The swap service of each CSI node plugin, reached over the operator's link.
//
// A JoinConsistencyGroup holds a moving volume's I/O on every host consuming
// it, and the plugin on that host is the only thing that can. Every plugin
// already links to the operator, so the swap is one more service on that
// session, and this is how a reconciler finds it by node name.

package volume

import (
	"errors"
	"fmt"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/volstack/swaprpc"
)

// peerRegistry is the part of the link registry the lookup reads.
type peerRegistry interface {
	Peer(id link.PeerID) (*link.Peer, error)
}

// linkSwapPeers finds a node's swap service in the link registry.
type linkSwapPeers struct {
	registry peerRegistry
	// remote builds the client for a linked peer. Nil is the gRPC client over
	// the peer's session.
	remote func(*link.Peer) SwapNode
}

// LinkSwapPeers is the swap service of the CSI node plugins linked to the
// operator.
func LinkSwapPeers(registry *link.Registry) SwapPeers {
	return linkSwapPeers{registry: registry}
}

// Node is the swap service on the named node and the plugin instance serving
// it. A node with no linked plugin is ErrNoSwapPeer, and a plugin that does not
// advertise the swap is ErrNoSwapCapability.
func (p linkSwapPeers) Node(name string) (SwapNode, string, error) {
	peer, err := p.registry.Peer(link.NodePeer(name))
	if errors.Is(err, link.ErrNoSession) {
		return nil, "", fmt.Errorf("%w: %s", ErrNoSwapPeer, name)
	}
	if err != nil {
		return nil, "", fmt.Errorf("find the CSI plugin on %s: %w", name, err)
	}
	if !peer.HasCapability(swaprpc.CapabilitySwap) {
		return nil, "", fmt.Errorf("%w: %s", ErrNoSwapCapability, name)
	}
	if p.remote != nil {
		return p.remote(peer), peer.InstanceUID, nil
	}
	return swaprpc.Remote(peer.Conn()), peer.InstanceUID, nil
}
