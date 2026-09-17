// linkAssembler is the NFSExportReconciler's ExportAssembler over csi-link: it
// turns a bound node name into a connection to that node and issues the export
// calls down it.
//
// It lives beside the reconciler rather than in atlas because it is the join
// between two things atlas keeps apart: the link registry, which knows which
// nodes are reachable, and the export service, which knows what to ask them.
// Neither should depend on the other, and the operator is where they meet.

package controller

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/export/exportrpc"
	"github.com/simplyblock/atlas/link"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// PeerRegistry is the part of the link hub this needs: a connection to one node,
// or link.ErrNoSession when that node is not currently attached. It is an
// interface so the join can be tested without standing up a hub.
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

// HasSession reports whether the node is attached right now.
//
// It is a separate question from whether a call fails, and the reconciler treats
// the two differently: a node that is merely disconnected during a rollout is a
// requeue, while a call that reached the node and failed is an error worth
// backing off on. Collapsing them would turn every rollout into a retry storm.
func (a *linkAssembler) HasSession(nodeName string) bool {
	_, err := a.registry.Conn(link.NodePeer(nodeName))
	return err == nil
}

// CreateExport assembles the export on the bound node.
func (a *linkAssembler) CreateExport(
	ctx context.Context,
	nodeName string,
	nfsExport *simplyblockv1alpha2.NFSExport,
) error {
	client, err := a.remote(nodeName)
	if err != nil {
		return err
	}
	spec, err := specFor(nfsExport)
	if err != nil {
		return err
	}
	return client.Create(ctx, spec)
}

// DeleteExport tears the export down on the bound node.
func (a *linkAssembler) DeleteExport(
	ctx context.Context,
	nodeName string,
	nfsExport *simplyblockv1alpha2.NFSExport,
) error {
	client, err := a.remote(nodeName)
	if err != nil {
		return err
	}
	spec, err := specFor(nfsExport)
	if err != nil {
		return err
	}
	return client.Delete(ctx, spec)
}

func (a *linkAssembler) remote(nodeName string) (*exportrpc.Client, error) {
	conn, err := a.registry.Conn(link.NodePeer(nodeName))
	if err != nil {
		if errors.Is(err, link.ErrNoSession) {
			// The reconciler asks HasSession first, so reaching here means the
			// session dropped between the two. Saying which it was keeps the
			// log honest about a race rather than reporting a node failure.
			return nil, fmt.Errorf("node %s went away mid-reconcile: %w", nodeName, err)
		}
		return nil, fmt.Errorf("reaching node %s: %w", nodeName, err)
	}
	return exportrpc.Remote(conn), nil
}

// specFor derives the host-side spec from the record.
//
// Everything it needs is already on the record: spec carries what the user asked
// for and status carries what provisioning observed, so nothing here re-derives
// an identifier or reaches back to the control plane. That is what makes a
// reconcile against a node cheap enough to run on every pass.
func specFor(nfsExport *simplyblockv1alpha2.NFSExport) (export.Spec, error) {
	if nfsExport.Status.NGUID == "" {
		// The namespace identifier is written by provisioning. Without it the
		// node cannot find the device, and guessing would be worse than waiting.
		return export.Spec{}, fmt.Errorf(
			"export %s: status.nguid is not set yet: %w",
			nfsExport.Name, export.ErrInvalidSpec)
	}
	clients := nfsExport.Status.AllowedClients
	if len(clients) == 0 {
		// An export with no client set publishes to nobody, which is a wait for
		// the policy to be resolved rather than a reason to widen it to
		// everyone. export.Spec.Validate refuses the empty set too; this says
		// so with the record's name attached.
		return export.Spec{}, fmt.Errorf(
			"export %s: no allowed clients resolved yet: %w",
			nfsExport.Name, export.ErrInvalidSpec)
	}
	return export.Spec{
		NGUID:   nfsExport.Status.NGUID,
		Path:    nfsExport.Spec.ExportPath,
		FSID:    nfsExport.Spec.FSID,
		Clients: clients,
	}, nil
}
