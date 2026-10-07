// The runner's side of the export service: it answers the operator's calls by
// handing them to the guest's agent, with the namespace's connection resolved
// on the way (design-pnfs-mds-vm.md §6.4).
//
// The resolving happens here because the guest cannot do it. It holds no
// cluster secret and no control-plane certificate by design, and boots from an
// image shared by every cluster, so the pod resolves the connection with its
// own credentials and the guest attaches with what it is handed. That makes
// this the one place DHCHAP secrets cross into the guest, over the pod's private
// bridge.

package relay

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

// Resolver asks the control plane where a spec's namespace is served and how
// the host named identity attaches it, and returns the identity the control
// plane named. nfsexport.PublishedConnection is the one the runner uses.
type Resolver func(ctx context.Context, spec export.Spec, identity string) (lvol.Connection, string, error)

// Relay hands export calls to the guest.
type Relay struct {
	// Guest is the agent's export service.
	Guest nfsexportrpc.Assembler
	// Resolve resolves a connection with the pod's credentials.
	Resolve Resolver
}

// Create assembles the export in the guest. The connection has to resolve:
// without one the guest finds no device, which names the wrong problem.
func (r Relay) Create(ctx context.Context, spec export.Spec) error {
	spec, err := r.withConnection(ctx, spec)
	if err != nil {
		return fmt.Errorf("resolving the connection for export %s: %w", spec.Path, err)
	}
	return r.Guest.Create(ctx, spec)
}

// Delete tears the export down in the guest, with a connection when the control
// plane answers and without one when it does not: the guest then releases the
// namespace its own stack record names, and a teardown has to work when the
// control plane does not.
func (r Relay) Delete(ctx context.Context, spec export.Spec) error {
	if resolved, err := r.withConnection(ctx, spec); err == nil {
		spec = resolved
	} else {
		spec.Connection = nil
	}
	return r.Guest.Delete(ctx, spec)
}

// Check asks the guest whether the export is served. It attaches nothing, so it
// needs no connection.
func (r Relay) Check(ctx context.Context, spec export.Spec) error {
	spec.Connection = nil
	return r.Guest.Check(ctx, spec)
}

// withConnection resolves the namespace's connection for the identity the
// operator decided. The control plane may name the identity itself, and its
// answer wins, since the connection was resolved for that one.
func (r Relay) withConnection(ctx context.Context, spec export.Spec) (export.Spec, error) {
	spec.Connection = nil
	connection, named, err := r.Resolve(ctx, spec, spec.HostNQN)
	if err != nil {
		return spec, err
	}
	if named != "" {
		spec.HostNQN = named
	}
	spec.Connection = &connection
	return spec, nil
}
