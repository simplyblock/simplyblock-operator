// The operator's way to a metadata server guest's MDSService: one client per
// pod, reached over the csi-link session that pod holds.
//
// It lives beside the hub because it is the join between the registry, which
// knows which pods are linked, and the role service, which knows what to ask
// them. The takeover decides and this only carries the call.

package csilink

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

const (
	// describeTimeout bounds a role query, which reads one variable in the guest.
	describeTimeout = 10 * time.Second
	// promoteTimeout bounds a promotion. It connects, fences, and mounts every
	// device, and starts nfsd, so it is sized for a cluster of many exports.
	promoteTimeout = 5 * time.Minute
)

// MDSRegistry is the part of the link registry the accessor needs.
type MDSRegistry interface {
	Conn(id link.PeerID) (grpc.ClientConnInterface, error)
}

// MDSGuests reaches the MDSService of each metadata server pod.
type MDSGuests struct {
	registry MDSRegistry
}

// NewMDSGuests returns the accessor over a registry.
func NewMDSGuests(registry MDSRegistry) *MDSGuests {
	return &MDSGuests{registry: registry}
}

// HasSession reports whether the pod has a live session now.
func (g *MDSGuests) HasSession(pod string) bool {
	_, err := g.registry.Conn(link.MDSPeer(pod))
	return err == nil
}

// Client is the pod's MDSService client, or the registry's error when the pod
// has no session.
func (g *MDSGuests) Client(pod string) (*nfsexportrpc.MDSClient, error) {
	conn, err := g.registry.Conn(link.MDSPeer(pod))
	if err != nil {
		return nil, fmt.Errorf("reaching metadata server pod %s: %w", pod, err)
	}
	return nfsexportrpc.RemoteMDS(conn), nil
}

// Describe asks the pod's guest for its role.
func (g *MDSGuests) Describe(ctx context.Context, pod string) (nfsexportrpc.Description, error) {
	c, err := g.Client(pod)
	if err != nil {
		return nfsexportrpc.Description{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	return c.Describe(ctx)
}

// Promote asks the pod's guest to become the active one.
func (g *MDSGuests) Promote(
	ctx context.Context, pod string, req nfsexportrpc.PromoteRequest,
) (nfsexportrpc.PromoteResult, error) {
	c, err := g.Client(pod)
	if err != nil {
		return nfsexportrpc.PromoteResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, promoteTimeout)
	defer cancel()
	return c.Promote(ctx, req)
}
