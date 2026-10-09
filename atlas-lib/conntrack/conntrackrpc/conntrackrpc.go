// Package conntrackrpc carries the conntrack flush over a link: the CSI node
// plugin serves it on the host's network, and the operator calls it.
//
// On the node:
//
//	srv, err := conntrackrpc.NewServer(conntrack.Forget)
//	cfg.Register = func(r grpc.ServiceRegistrar) { srv.Register(r) }
//	cfg.Capabilities = append(cfg.Capabilities, conntrackrpc.Capabilities()...)
//
// From the operator, per node peer that advertises the capability:
//
//	n, err := conntrackrpc.Remote(peer.Conn()).Forget(ctx, sel)
package conntrackrpc

import (
	"context"
	"fmt"
	"net/netip"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/conntrack"
	"github.com/simplyblock/atlas/conntrack/conntrackrpc/conntrackv1"
	"github.com/simplyblock/atlas/errs/class"
)

// CapabilityFlows is what a node advertises when it can forget flows.
const CapabilityFlows = "atlas.conntrack.v1.FlowService"

// Capabilities names what this package serves, for the link handshake.
func Capabilities() []string { return []string{CapabilityFlows} }

// ForgetFunc does the node-side work. conntrack.Forget is the real one.
type ForgetFunc func(conntrack.Selector) (uint, error)

// Server serves FlowService over a link.
type Server struct {
	conntrackv1.UnimplementedFlowServiceServer
	forget ForgetFunc
}

// NewServer refuses a nil function rather than failing on the first call.
func NewServer(forget ForgetFunc) (*Server, error) {
	if forget == nil {
		return nil, fmt.Errorf("conntrackrpc: no forget function")
	}
	return &Server{forget: forget}, nil
}

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	conntrackv1.RegisterFlowServiceServer(r, s)
}

// ForgetFlows deletes the selected flows on this node.
func (s *Server) ForgetFlows(
	_ context.Context, req *conntrackv1.ForgetFlowsRequest,
) (*conntrackv1.ForgetFlowsResponse, error) {
	sel, err := selectorFromProto(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	n, err := s.forget(sel)
	if err != nil {
		return nil, class.Status(err)
	}
	return &conntrackv1.ForgetFlowsResponse{Deleted: uint64(n)}, nil
}

// Client reaches one node's FlowService.
type Client struct {
	client conntrackv1.FlowServiceClient
}

// Remote returns a Client over an established link connection.
func Remote(conn grpc.ClientConnInterface) *Client {
	return &Client{client: conntrackv1.NewFlowServiceClient(conn)}
}

// Forget deletes the selected flows on the far node and returns how many it
// deleted. An invalid selector is refused before anything is sent.
func (c *Client) Forget(ctx context.Context, sel conntrack.Selector) (uint, error) {
	if err := sel.Validate(); err != nil {
		return 0, err
	}
	resp, err := c.client.ForgetFlows(ctx, forgetRequest(sel))
	if err != nil {
		return 0, fmt.Errorf("node: forget flows to %s: %w", sel.ReplySource, class.FromStatus(err))
	}
	return uint(resp.GetDeleted()), nil
}

func forgetRequest(sel conntrack.Selector) *conntrackv1.ForgetFlowsRequest {
	req := &conntrackv1.ForgetFlowsRequest{Protocol: uint32(sel.Protocol), DstPort: uint32(sel.DstPort)}
	if sel.ReplySource.IsValid() {
		req.ReplySource = sel.ReplySource.String()
	}
	return req
}

func selectorFromProto(req *conntrackv1.ForgetFlowsRequest) (conntrack.Selector, error) {
	if req.GetProtocol() > 255 || req.GetDstPort() > 65535 {
		return conntrack.Selector{}, fmt.Errorf("%w: protocol %d, port %d out of range",
			conntrack.ErrInvalidSelector, req.GetProtocol(), req.GetDstPort())
	}
	sel := conntrack.Selector{
		Protocol: conntrack.Protocol(req.GetProtocol()),
		DstPort:  uint16(req.GetDstPort()),
	}
	if s := req.GetReplySource(); s != "" {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return conntrack.Selector{}, fmt.Errorf("%w: reply source %q: %v", conntrack.ErrInvalidSelector, s, err)
		}
		sel.ReplySource = addr
	}
	return sel, sel.Validate()
}
