// Package exportrpc carries pNFS export assembly over a link: the node serves
// it, and the operator calls it.
//
// It is the mutating counterpart to storagerpc, and deliberately a separate
// service rather than more methods on that one. storagerpc is read-only by
// construction -- its own documentation says attaching and detaching fabrics
// belongs to a separate, mutating service so a credential can be granted the one
// and not the other -- and everything here writes to the node.
//
// On the node:
//
//	assembler, err := export.New(export.Config{ ... })
//	srv := exportrpc.NewServer(assembler)
//	agent, err := link.NewAgent(link.AgentConfig{
//	    Register:     srv.Register,
//	    Capabilities: exportrpc.Capabilities(),
//	    // ...
//	})
//
// In the operator:
//
//	conn, err := hub.Registry().Conn(link.NodePeer(nodeName))
//	if errors.Is(err, link.ErrNoSession) {
//	    return ctrl.Result{RequeueAfter: backoff}, nil  // not a failure
//	}
//	err = exportrpc.Remote(conn).Create(ctx, spec)
//
// # Why the node is the server
//
// The direction is the reverse of how it reads. The node dials the operator and
// holds the connection open, and the operator issues these calls back down it,
// so nothing has to listen on a node. That is the link's arrangement, not this
// package's, and the consequence worth knowing is that a node with no live
// session is a normal state rather than an error: it is legitimately
// disconnected during a rollout. Callers get link.ErrNoSession and requeue.
package exportrpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/export/exportrpc/exportv1"
)

// CapabilityExports is the capability a node advertises when it can assemble
// exports. A node that does not serve this is one the operator must not bind an
// export to, and naming it is what lets the operator tell that apart from a node
// that is merely disconnected.
const CapabilityExports = "atlas.export.v1.ExportService"

// Capabilities names what this package serves, for the link handshake.
func Capabilities() []string { return []string{CapabilityExports} }

// Assembler is the node-side work this service exposes. export.Assembler
// satisfies it; a test passes one that records.
type Assembler interface {
	Create(ctx context.Context, spec export.Spec) error
	Delete(ctx context.Context, spec export.Spec) error
}

// Server serves ExportService over a link.
type Server struct {
	exportv1.UnimplementedExportServiceServer
	assembler Assembler
}

// NewServer wraps an assembler. It refuses a nil one rather than failing on the
// first call, because a node that registered the capability and cannot honor it
// is worse than a node that never registered it.
func NewServer(assembler Assembler) (*Server, error) {
	if assembler == nil {
		return nil, fmt.Errorf("exportrpc: no assembler")
	}
	return &Server{assembler: assembler}, nil
}

// Register adds the service to a gRPC server, which is what a link agent does
// with the services its peer may call.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	exportv1.RegisterExportServiceServer(r, s)
}

// CreateExport assembles the export on this node.
func (s *Server) CreateExport(
	ctx context.Context, req *exportv1.CreateExportRequest,
) (*exportv1.CreateExportResponse, error) {
	if err := s.assembler.Create(ctx, specFromProto(req.GetSpec())); err != nil {
		return nil, class.Status(err)
	}
	return &exportv1.CreateExportResponse{}, nil
}

// DeleteExport tears the export down on this node.
func (s *Server) DeleteExport(
	ctx context.Context, req *exportv1.DeleteExportRequest,
) (*exportv1.DeleteExportResponse, error) {
	if err := s.assembler.Delete(ctx, specFromProto(req.GetSpec())); err != nil {
		return nil, class.Status(err)
	}
	return &exportv1.DeleteExportResponse{}, nil
}

// Client reaches one node's ExportService.
type Client struct {
	client exportv1.ExportServiceClient
}

// Remote returns a Client over an established link connection.
func Remote(conn grpc.ClientConnInterface) *Client {
	return &Client{client: exportv1.NewExportServiceClient(conn)}
}

// Create assembles the export on the far node.
func (c *Client) Create(ctx context.Context, spec export.Spec) error {
	_, err := c.client.CreateExport(ctx, &exportv1.CreateExportRequest{Spec: specToProto(spec)})
	if err != nil {
		return fmt.Errorf("node: create export %s: %w", spec.Path, class.FromStatus(err))
	}
	return nil
}

// Delete tears the export down on the far node.
func (c *Client) Delete(ctx context.Context, spec export.Spec) error {
	_, err := c.client.DeleteExport(ctx, &exportv1.DeleteExportRequest{Spec: specToProto(spec)})
	if err != nil {
		return fmt.Errorf("node: delete export %s: %w", spec.Path, class.FromStatus(err))
	}
	return nil
}

func specToProto(s export.Spec) *exportv1.ExportSpec {
	return &exportv1.ExportSpec{
		VolumeUuid: s.VolumeUUID,
		Path:       s.Path,
		Fsid:       s.FSID,
		Clients:    s.Clients,
	}
}

func specFromProto(s *exportv1.ExportSpec) export.Spec {
	if s == nil {
		return export.Spec{}
	}
	return export.Spec{
		VolumeUUID: s.GetVolumeUuid(),
		Path:       s.GetPath(),
		FSID:       s.GetFsid(),
		Clients:    s.GetClients(),
	}
}
