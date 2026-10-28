// Asking a worker's CSI node plugin what its host network shows: the addresses
// of a NIC before a storage pod takes it, and whether a connection from the host
// to a storage listener succeeds (design-storage-network.md §8.1).
//
// It lives beside the hub because it is a call down the connection a node
// plugin holds, the way asking every node to forget flows is.

package csilink

import (
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/netcheck"
	"github.com/simplyblock/atlas/netcheck/netcheckrpc"
)

// checkTimeout bounds one call to a node, and connectTimeout the connection
// attempt the node makes.
const (
	checkTimeout   = 10 * time.Second
	connectTimeout = 5 * time.Second
)

// HostCheck asks the node plugins of the workers. It satisfies the storage node
// reconciler's HostChecker.
type HostCheck struct {
	peers   func() []NodePeer
	read    func(ctx context.Context, conn grpc.ClientConnInterface, sel netcheck.Selector) (netcheck.Report, error)
	connect func(ctx context.Context, conn grpc.ClientConnInterface, address string, port int, timeout time.Duration) (netcheckrpc.Result, error)
}

// NewHostCheck returns a HostCheck over the given peer listing.
func NewHostCheck(peers func() []NodePeer) *HostCheck {
	return &HostCheck{
		peers: peers,
		read: func(ctx context.Context, conn grpc.ClientConnInterface, sel netcheck.Selector) (netcheck.Report, error) {
			return netcheckrpc.Remote(conn).InterfaceAddresses(ctx, sel)
		},
		connect: func(ctx context.Context, conn grpc.ClientConnInterface, address string, port int, timeout time.Duration) (netcheckrpc.Result, error) {
			return netcheckrpc.Remote(conn).Connect(ctx, address, port, timeout)
		},
	}
}

// peerOf is the capable plugin linked on a worker, or ErrUnavailable. A plugin
// that is linked and does not advertise the check is an older one, and is as
// unavailable as one that is not linked.
func (h *HostCheck) peerOf(worker string) (NodePeer, error) {
	for _, p := range h.peers() {
		if p.Name == worker && p.CanCheck {
			return p, nil
		}
	}
	return NodePeer{}, fmt.Errorf("worker %s: %w", worker, netcheckrpc.ErrUnavailable)
}

// InterfaceAddresses reads one interface of a worker's host.
func (h *HostCheck) InterfaceAddresses(ctx context.Context, worker string, sel netcheck.Selector) (netcheck.Report, error) {
	p, err := h.peerOf(worker)
	if err != nil {
		return netcheck.Report{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	return h.read(ctx, p.Conn, sel)
}

// Connect asks a worker's host to open a TCP connection, and says whether it
// connected and why not when it did not.
func (h *HostCheck) Connect(ctx context.Context, worker, address string, port int) (bool, string, error) {
	p, err := h.peerOf(worker)
	if err != nil {
		return false, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	result, err := h.connect(ctx, p.Conn, address, port, connectTimeout)
	if err != nil {
		return false, "", err
	}
	return result.Connected, result.Detail, nil
}

// Workers lists the workers that have a plugin linked that answers host checks.
func (h *HostCheck) Workers() []string {
	var workers []string
	for _, p := range h.peers() {
		if p.CanCheck {
			workers = append(workers, p.Name)
		}
	}
	slices.Sort(workers)
	return workers
}
