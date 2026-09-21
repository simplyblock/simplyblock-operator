// Package exportrpc carries pNFS export assembly over a link: the node serves
// it, and the operator calls it.
//
// The mutating counterpart to storagerpc, and a separate service rather than
// more methods on it: storagerpc is read-only by construction, and keeping them
// apart lets a credential be granted the reading and not the writing.
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
// The direction reads backward: the node dials the operator and the operator
// calls back down it. So a node with no live session is normal rather than an
// error, and callers get link.ErrNoSession and requeue.
package exportrpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/export/exportrpc/exportv1"
)

// CapabilityExports is what a node advertises when it can assemble exports.
const CapabilityExports = "atlas.export.v1.ExportService"

// Capabilities names what this package serves, for the link handshake.
func Capabilities() []string { return []string{CapabilityExports} }

// Assembler is the node-side work. export.Assembler satisfies it.
type Assembler interface {
	Create(ctx context.Context, spec export.Spec) error
	Delete(ctx context.Context, spec export.Spec) error
}

// Server serves ExportService over a link.
type Server struct {
	exportv1.UnimplementedExportServiceServer
	assembler Assembler
}

// NewServer refuses a nil assembler rather than failing on the first call: a
// node that advertises the capability and cannot honor it is worse than one
// that never advertised it.
func NewServer(assembler Assembler) (*Server, error) {
	if assembler == nil {
		return nil, fmt.Errorf("exportrpc: no assembler")
	}
	return &Server{assembler: assembler}, nil
}

// Register adds the service to a gRPC server.
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
		ClusterId:  s.ClusterID,
		PoolId:     s.PoolID,
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
		ClusterID:  s.GetClusterId(),
		PoolID:     s.GetPoolId(),
		Path:       s.GetPath(),
		FSID:       s.GetFsid(),
		Clients:    s.GetClients(),
	}
}
