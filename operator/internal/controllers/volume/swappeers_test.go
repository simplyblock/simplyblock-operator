package volume

import (
	"errors"
	"testing"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/volstack/swaprpc"
)

type peerTable map[string]*link.Peer

func (t peerTable) Peer(id link.PeerID) (*link.Peer, error) {
	if p, ok := t[id.Name]; ok && id.Kind == link.PeerKindNode {
		return p, nil
	}
	return nil, link.ErrNoSession
}

func TestANodeWithNoLinkedPluginHasNoSwapPeer(t *testing.T) {
	peers := linkSwapPeers{registry: peerTable{}}
	if _, _, err := peers.Node("worker-9"); !errors.Is(err, ErrNoSwapPeer) {
		t.Errorf("err = %v, want ErrNoSwapPeer", err)
	}
}

func TestAPluginWithoutTheSwapCapabilityIsRefused(t *testing.T) {
	peers := linkSwapPeers{registry: peerTable{"worker-2": {ID: link.NodePeer("worker-2")}}}
	if _, _, err := peers.Node("worker-2"); !errors.Is(err, ErrNoSwapCapability) {
		t.Errorf("err = %v, want ErrNoSwapCapability", err)
	}
}

func TestACapablePluginIsReachedWithItsInstance(t *testing.T) {
	peer := &link.Peer{ID: link.NodePeer("worker-2"), InstanceUID: "i-1", Capabilities: []string{swaprpc.CapabilitySwap}}
	reached := ""
	peers := linkSwapPeers{
		registry: peerTable{"worker-2": peer},
		remote:   func(p *link.Peer) SwapNode { reached = p.InstanceUID; return nil },
	}
	_, instance, err := peers.Node("worker-2")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "i-1" || reached != "i-1" {
		t.Errorf("instance = %q, reached = %q, want i-1", instance, reached)
	}
}
