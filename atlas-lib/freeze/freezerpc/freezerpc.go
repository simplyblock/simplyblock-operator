// Package freezerpc carries a striped volume's freeze over the link, from the
// CSI controller through the operator to the CSI node plugin that stages the
// volume (atlas-lib/freeze, csi-driver/docs/striped-volumes.md).
//
// The controller takes a striped volume's member snapshots, and only the node
// that stages the volume can stop its writes. The link runs from each CSI pod to
// the operator, so the controller cannot reach a node directly: it calls the
// operator's relay, naming the node, and the relay passes the call down that
// node's link.
//
// On the node:
//
//	srv, err := freezerpc.NewServer(freezer)
//	cfg.Register = func(r grpc.ServiceRegistrar) { srv.Register(r) }
//	cfg.Capabilities = append(cfg.Capabilities, freezerpc.Capabilities()...)
//
// On the operator, beside the other services it serves its peers:
//
//	relay := freezerpc.NewRelay(func(node string) (grpc.ClientConnInterface, error) {
//	    return registry.Conn(link.NodePeer(node))
//	})
//
// On the CSI controller:
//
//	conn, err := agent.Conn()
//	frozen, err := freezerpc.Remote(conn).Freeze(ctx, node, handle, token, deadline)
package freezerpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/freeze/freezerpc/freezev1"
	"github.com/simplyblock/atlas/link"
)

// CapabilityFreeze is what a node advertises when it can freeze a striped
// volume, and what the operator advertises when it relays the call.
const CapabilityFreeze = "atlas.freeze.v1.FreezeService"

// Capabilities names what this package serves, for the link handshake.
func Capabilities() []string { return []string{CapabilityFreeze} }

// Freezer is the node-side work: suspend the striped device of the volume a
// handle names, and resume it.
type Freezer interface {
	// Freeze reports false, with no error, for a volume this node does not
	// stage.
	Freeze(ctx context.Context, handle, token string, deadline time.Duration) (bool, error)
	// Thaw reports whether the freeze held from Freeze until now.
	Thaw(ctx context.Context, handle, token string) (bool, error)
}

// Server serves FreezeService on a node.
type Server struct {
	freezev1.UnimplementedFreezeServiceServer
	freezer Freezer
}

// NewServer refuses a nil freezer rather than failing on the first call.
func NewServer(freezer Freezer) (*Server, error) {
	if freezer == nil {
		return nil, errors.New("freezerpc: no freezer")
	}
	return &Server{freezer: freezer}, nil
}

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	freezev1.RegisterFreezeServiceServer(r, s)
}

// Freeze suspends the volume on this node.
func (s *Server) Freeze(ctx context.Context, req *freezev1.FreezeRequest) (*freezev1.FreezeResponse, error) {
	if req.GetVolumeHandle() == "" || req.GetToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "freeze: a volume handle and a token are required")
	}
	frozen, err := s.freezer.Freeze(ctx, req.GetVolumeHandle(), req.GetToken(),
		time.Duration(req.GetDeadlineMs())*time.Millisecond)
	if err != nil {
		return nil, asStatus(err)
	}
	return &freezev1.FreezeResponse{Frozen: frozen}, nil
}

// Thaw resumes the volume on this node.
func (s *Server) Thaw(ctx context.Context, req *freezev1.ThawRequest) (*freezev1.ThawResponse, error) {
	if req.GetVolumeHandle() == "" || req.GetToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "thaw: a volume handle and a token are required")
	}
	held, err := s.freezer.Thaw(ctx, req.GetVolumeHandle(), req.GetToken())
	if err != nil && !held {
		return nil, asStatus(err)
	}
	// A hold that lasted and could not be resumed is reported as held: what
	// the caller took under it is consistent, and the node keeps the resume.
	return &freezev1.ThawResponse{Held: held}, nil
}

// asStatus keeps a status the freezer chose and makes anything else Internal.
func asStatus(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}

// Relay serves FreezeService on the operator and passes each call on to the
// node it names.
type Relay struct {
	freezev1.UnimplementedFreezeServiceServer
	conn func(node string) (grpc.ClientConnInterface, error)
}

// NewRelay relays over conn, which answers the link connection to a node.
func NewRelay(conn func(node string) (grpc.ClientConnInterface, error)) *Relay {
	return &Relay{conn: conn}
}

// Register adds the relay to a gRPC server.
func (r *Relay) Register(s grpc.ServiceRegistrar) {
	freezev1.RegisterFreezeServiceServer(s, r)
}

// Freeze relays a freeze from a CSI controller to the node it names.
func (r *Relay) Freeze(ctx context.Context, req *freezev1.FreezeRequest) (*freezev1.FreezeResponse, error) {
	client, err := r.node(ctx, req.GetNode())
	if err != nil {
		return nil, err
	}
	return client.Freeze(ctx, req)
}

// Thaw relays a thaw from a CSI controller to the node it names.
func (r *Relay) Thaw(ctx context.Context, req *freezev1.ThawRequest) (*freezev1.ThawResponse, error) {
	client, err := r.node(ctx, req.GetNode())
	if err != nil {
		return nil, err
	}
	return client.Thaw(ctx, req)
}

// node admits a CSI controller only, since suspending a volume stalls the
// workload on it, and reaches the named node.
func (r *Relay) node(ctx context.Context, node string) (freezev1.FreezeServiceClient, error) {
	caller, ok := link.PeerFromContext(ctx)
	if !ok || caller.Kind != link.PeerKindController {
		return nil, status.Errorf(codes.PermissionDenied,
			"freeze: only a CSI controller freezes volumes, and %s is not one", caller)
	}
	if node == "" {
		return nil, status.Error(codes.InvalidArgument, "freeze: no node named")
	}
	if r.conn == nil {
		return nil, status.Error(codes.Unavailable, "freeze: the operator's link is not up")
	}
	conn, err := r.conn(node)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "freeze: node %s is not linked: %v", node, err)
	}
	return freezev1.NewFreezeServiceClient(conn), nil
}

// Client reaches FreezeService, on the operator's relay or on a node.
type Client struct {
	client freezev1.FreezeServiceClient
}

// Remote returns a Client over an established link connection.
func Remote(conn grpc.ClientConnInterface) *Client {
	return &Client{client: freezev1.NewFreezeServiceClient(conn)}
}

// Freeze asks node to suspend the volume for at most deadline. It reports
// false for a volume the node does not stage.
func (c *Client) Freeze(
	ctx context.Context, node, handle, token string, deadline time.Duration,
) (bool, error) {
	resp, err := c.client.Freeze(ctx, &freezev1.FreezeRequest{
		Node: node, VolumeHandle: handle, Token: token, DeadlineMs: deadline.Milliseconds(),
	})
	if err != nil {
		return false, fmt.Errorf("freeze %s on %s: %w", handle, node, err)
	}
	return resp.GetFrozen(), nil
}

// Thaw asks node to resume the volume, and reports whether the freeze held.
func (c *Client) Thaw(ctx context.Context, node, handle, token string) (bool, error) {
	resp, err := c.client.Thaw(ctx, &freezev1.ThawRequest{Node: node, VolumeHandle: handle, Token: token})
	if err != nil {
		return false, fmt.Errorf("thaw %s on %s: %w", handle, node, err)
	}
	return resp.GetHeld(), nil
}
