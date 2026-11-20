// linkAssembler turns a bound host into a connection and issues the export
// calls down it.
//
// It lives beside the reconciler rather than in atlas because it joins two
// things atlas keeps apart: the link registry, which knows which nodes are
// reachable, and the export service, which knows what to ask them.

package controller

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/link"
	atlaslvol "github.com/simplyblock/atlas/lvol"
	export "github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// PeerRegistry is the part of the link hub this needs. An interface, so the
// join is testable without standing up a hub.
type PeerRegistry interface {
	Conn(id link.PeerID) (grpc.ClientConnInterface, error)
}

// linkAssembler implements ExportAssembler over the link.
type linkAssembler struct {
	registry PeerRegistry
}

// NewLinkAssembler returns an ExportAssembler that reaches nodes over csi-link.
func NewLinkAssembler(registry PeerRegistry) ExportAssembler {
	return &linkAssembler{registry: registry}
}

// HasSession reports whether the host is attached right now.
//
// A separate question from whether a call fails, and the reconciler treats the
// two differently: collapsing them turns every rollout into a retry storm.
func (a *linkAssembler) HasSession(host link.PeerID) bool {
	_, err := a.registry.Conn(host)
	return err == nil
}

// CreateExport assembles the export on the bound host.
func (a *linkAssembler) CreateExport(
	ctx context.Context,
	host ExportHost,
	nfsExport *simplyblockv1alpha2.NFSExport,
) error {
	conn, err := a.conn(host.Peer)
	if err != nil {
		return err
	}
	spec, err := specFor(nfsExport, host.HostNQN)
	if err != nil {
		return err
	}
	return nfsexportrpc.Remote(conn).Create(ctx, spec)
}

// DeleteExport tears the export down on the bound host.
func (a *linkAssembler) DeleteExport(
	ctx context.Context,
	host ExportHost,
	nfsExport *simplyblockv1alpha2.NFSExport,
) error {
	conn, err := a.conn(host.Peer)
	if err != nil {
		return err
	}
	spec, err := specFor(nfsExport, host.HostNQN)
	if err != nil {
		return err
	}
	return nfsexportrpc.Remote(conn).Delete(ctx, spec)
}

// CheckExport asks the bound host whether the export is actually being
// served.
func (a *linkAssembler) CheckExport(
	ctx context.Context,
	host ExportHost,
	nfsExport *simplyblockv1alpha2.NFSExport,
) error {
	conn, err := a.conn(host.Peer)
	if err != nil {
		return err
	}
	spec, err := specFor(nfsExport, host.HostNQN)
	if err != nil {
		return err
	}
	return nfsexportrpc.Remote(conn).Check(ctx, spec)
}

// conn is the link connection to one host.
func (a *linkAssembler) conn(host link.PeerID) (grpc.ClientConnInterface, error) {
	conn, err := a.registry.Conn(host)
	if err != nil {
		if errors.Is(err, link.ErrNoSession) {
			// HasSession was asked first, so the session dropped between the
			// two. Saying so keeps the log honest about a race.
			return nil, fmt.Errorf("%s went away mid-reconcile: %w", host, err)
		}
		return nil, fmt.Errorf("reaching %s: %w", host, err)
	}
	return conn, nil
}

// specFor derives the host-side spec from the record. Everything it needs is
// on the spec, so nothing reaches the control plane and nothing waits on a
// status field provisioning might not have written yet.
func specFor(nfsExport *simplyblockv1alpha2.NFSExport, hostNQN string) (export.Spec, error) {
	clients := nfsExport.Status.AllowedClients
	if len(clients) == 0 {
		// An export with no clients publishes to nobody: a wait, not a reason
		// to widen it to everyone.
		return export.Spec{}, fmt.Errorf(
			"export %s: no allowed clients resolved yet: %w",
			nfsExport.Name, export.ErrInvalidSpec)
	}
	// Everything else comes from the handle. The volume's own id is the backing
	// namespace UUID and the fsid both, so reading all three from one string is
	// what makes them unable to disagree.
	handle, ok := atlaslvol.ParseHandle(atlaslvol.VolumeHandle(nfsExport.Spec.VolumeRef))
	if !ok {
		// Refused rather than passed on half-filled: a host with no cluster
		// reports a missing device, which names the wrong problem.
		return export.Spec{}, fmt.Errorf(
			"export %s: volumeRef %q is not a volume handle: %w",
			nfsExport.Name, nfsExport.Spec.VolumeRef, export.ErrInvalidSpec)
	}

	return export.Spec{
		VolumeUUID: handle.VolumeID,
		ClusterID:  handle.ClusterID,
		PoolID:     handle.PoolRef,
		Path:       nfsExport.Spec.ExportPath,
		FSID:       handle.VolumeID,
		Encrypted:  nfsExport.Spec.Encrypted,
		Clients:    clients,
		HostNQN:    hostNQN,
	}, nil
}
