// The runner's side of the role service: it relays Describe and Promote to the
// guest's agent, resolving every volume's connection with the pod's own host
// NQN on the way (design-pnfs-mds-standby.md §8).
//
// It lives beside the export relay because it answers the same question for the
// same reason: the guest holds no control-plane credential, so the pod resolves
// what the guest connects.

package relay

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// MDS hands role calls to the guest.
type MDS struct {
	// Guest is the agent's role service.
	Guest nfsexportrpc.MDS
	// Tracker resolves connections and remembers the exports a promotion assembled.
	Tracker *Tracker
}

// Describe asks the guest for its role.
func (m MDS) Describe(ctx context.Context) (nfsexportrpc.Description, error) {
	return m.Guest.Describe(ctx)
}

// Promote resolves every connection for the pod's host NQN, drops whatever the
// caller sent, and hands the guest the promotion. A promotion that makes the
// guest active leaves its exports in the tracker, so the endpoint refresh and
// path repair cover them from then on. The outcome and any guest error return
// as the guest gave them.
func (m MDS) Promote(ctx context.Context, req nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
	resolved, conns, err := m.withConnections(ctx, req)
	if err != nil {
		return nfsexportrpc.PromoteResult{}, err
	}
	result, err := m.Guest.Promote(ctx, resolved)
	if err != nil {
		return result, err
	}
	if result.Outcome == nfsexportrpc.PromoteActive {
		for i, spec := range resolved.Exports {
			m.Tracker.Assembled(spec, conns[i])
		}
	}
	return result, nil
}

// withConnections returns the request with a connection resolved for the pod's
// host NQN on the state volume and on every export, and the exports'
// connections in order. One identity governs the fence keys of a promotion, so a
// volume the control plane resolves for another identity fails the call.
func (m MDS) withConnections(
	ctx context.Context, req nfsexportrpc.PromoteRequest,
) (nfsexportrpc.PromoteRequest, []lvol.Connection, error) {
	state := export.Spec{
		VolumeUUID: req.StateVolume.VolumeUUID, ClusterID: req.StateVolume.ClusterID,
		PoolID: req.StateVolume.PoolID, Path: "the state volume",
	}
	conn, err := m.resolve(ctx, state, req.HostNQN)
	if err != nil {
		return req, nil, err
	}
	req.StateVolume.Connection = &conn

	exports := make([]export.Spec, len(req.Exports))
	conns := make([]lvol.Connection, len(req.Exports))
	for i, spec := range req.Exports {
		spec.HostNQN = req.HostNQN
		if conns[i], err = m.resolve(ctx, spec, req.HostNQN); err != nil {
			return req, nil, err
		}
		spec.Connection = &conns[i]
		exports[i] = spec
	}
	req.Exports = exports
	return req, conns, nil
}

func (m MDS) resolve(ctx context.Context, spec export.Spec, hostNQN string) (lvol.Connection, error) {
	spec.Connection = nil
	conn, named, err := m.Tracker.Relay.Resolve(ctx, spec, hostNQN)
	if err != nil {
		return lvol.Connection{}, fmt.Errorf("resolving the connection for %s: %w", spec.Path, err)
	}
	if named != "" && named != hostNQN {
		return lvol.Connection{}, fmt.Errorf("the control plane resolved %s for host %s, not the pod's %s",
			spec.Path, named, hostNQN)
	}
	return conn, nil
}
