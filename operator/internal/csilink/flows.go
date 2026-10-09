// Asking every CSI node plugin to forget the NFS flows translated to a
// metadata server address that is gone (design-pnfs-mds-vm.md §8.1).
//
// It lives beside the hub because it is a fan-out over the hub's node peers,
// and the NFSExport reconciler only says which address: a reconcile never
// waits on it, and a node that is not linked or fails is logged and skipped.

package csilink

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/simplyblock/atlas/conntrack"
	"github.com/simplyblock/atlas/conntrack/conntrackrpc"
	"github.com/simplyblock/atlas/link"
)

const (
	// nfsPort is the port every export's Service exposes.
	nfsPort = 2049
	// forgetDelay gives kube-proxy time to apply the EndpointSlice change
	// before the flows go: a flow forgotten while the dead address is still
	// listed would be translated to it again by the client's next SYN.
	forgetDelay = 2 * time.Second
	// forgetTimeout bounds one node's call.
	forgetTimeout = 10 * time.Second
)

// NodePeer is one linked node plugin as Flows sees it.
type NodePeer struct {
	Name      string
	CanForget bool
	Conn      grpc.ClientConnInterface
}

// RegistryNodePeers lists the registry's node peers.
func RegistryNodePeers(reg *link.Registry) func() []NodePeer {
	return func() []NodePeer {
		peers := reg.PeersOfKind(link.PeerKindNode)
		out := make([]NodePeer, 0, len(peers))
		for _, p := range peers {
			out = append(out, NodePeer{
				Name:      p.ID.Name,
				CanForget: p.HasCapability(conntrackrpc.CapabilityFlows),
				Conn:      p.Conn(),
			})
		}
		return out
	}
}

// Flows asks the node plugins to forget flows. It satisfies the NFSExport
// reconciler's FlowForgetter.
type Flows struct {
	peers  func() []NodePeer
	delay  time.Duration
	forget func(ctx context.Context, node string, conn grpc.ClientConnInterface, sel conntrack.Selector) (uint, error)

	mu      sync.Mutex
	pending map[netip.Addr]bool
}

// NewFlows returns Flows over the given peer listing.
func NewFlows(peers func() []NodePeer) *Flows {
	return &Flows{
		peers: peers,
		delay: forgetDelay,
		forget: func(ctx context.Context, _ string, conn grpc.ClientConnInterface, sel conntrack.Selector) (uint, error) {
			return conntrackrpc.Remote(conn).Forget(ctx, sel)
		},
		pending: map[netip.Addr]bool{},
	}
}

// Forget asks every capable node, after a short delay, to forget the TCP flows
// to the NFS port translated to the given address. It returns at once. A
// request for an address already waiting is folded into that one.
func (f *Flows) Forget(ip string) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return
	}
	addr = addr.Unmap()
	f.mu.Lock()
	if f.pending[addr] {
		f.mu.Unlock()
		return
	}
	f.pending[addr] = true
	f.mu.Unlock()

	go func() {
		time.Sleep(f.delay)
		f.mu.Lock()
		delete(f.pending, addr)
		f.mu.Unlock()
		f.fanOut(conntrack.Selector{Protocol: conntrack.TCP, DstPort: nfsPort, ReplySource: addr})
	}()
}

func (f *Flows) fanOut(sel conntrack.Selector) {
	log := ctrl.Log.WithName("csi-link").WithValues("address", sel.ReplySource.String())
	var wg sync.WaitGroup
	for _, p := range f.peers() {
		if !p.CanForget {
			continue
		}
		wg.Add(1)
		go func(p NodePeer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), forgetTimeout)
			defer cancel()
			n, err := f.forget(ctx, p.Name, p.Conn, sel)
			if err != nil {
				log.Error(err, "a node could not forget the flows to a replaced metadata server", "node", p.Name)
				return
			}
			if n > 0 {
				log.Info("a node forgot the flows to a replaced metadata server", "node", p.Name, "flows", n)
			}
		}(p)
	}
	wg.Wait()
}
