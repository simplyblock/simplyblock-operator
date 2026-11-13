// Package netcheckrpc carries the host network checks over a link: the CSI node
// plugin serves them on the host's network, and the operator calls them.
//
// On the node:
//
//	srv, err := netcheckrpc.NewServer(read, connect)
//	cfg.Register = func(r grpc.ServiceRegistrar) { srv.Register(r) }
//	cfg.Capabilities = append(cfg.Capabilities, netcheckrpc.Capabilities()...)
//
// From the operator, per node peer that advertises the capability:
//
//	report, err := netcheckrpc.Remote(peer.Conn()).InterfaceAddresses(ctx, sel)
package netcheckrpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/netcheck"
	"github.com/simplyblock/atlas/netcheck/netcheckrpc/netcheckv1"
)

// CapabilityCheck is what a node advertises when it can answer host checks.
const CapabilityCheck = "atlas.netcheck.v1.CheckService"

// ErrUnavailable says no linked node can answer a check for the worker asked
// about: its plugin is not linked, or does not advertise CapabilityCheck.
var ErrUnavailable = errors.New("netcheckrpc: no linked node can answer the host check")

// Capabilities names what this package serves, for the link handshake.
func Capabilities() []string { return []string{CapabilityCheck} }

const (
	// defaultTimeout is how long a connection attempt may take when the caller
	// names no time, and maxTimeout is the longest the node allows, so a caller
	// cannot hold the host's check open.
	defaultTimeout = 5 * time.Second
	maxTimeout     = 30 * time.Second
)

// ReadFunc reads one host interface. netcheck.Interface is the real one.
type ReadFunc func(netcheck.Selector) (netcheck.Report, error)

// ConnectFunc opens a TCP connection from the host. netcheck.Connect is the real one.
type ConnectFunc func(ctx context.Context, address string, port int) error

// Server serves CheckService over a link.
type Server struct {
	netcheckv1.UnimplementedCheckServiceServer
	read    ReadFunc
	connect ConnectFunc
}

// NewServer refuses a nil function rather than failing on the first call.
func NewServer(read ReadFunc, connect ConnectFunc) (*Server, error) {
	if read == nil || connect == nil {
		return nil, errors.New("netcheckrpc: a read and a connect function are both required")
	}
	return &Server{read: read, connect: connect}, nil
}

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	netcheckv1.RegisterCheckServiceServer(r, s)
}

// InterfaceAddresses reads one interface of this node's host. An interface the
// host lacks is an answer, not a failure.
func (s *Server) InterfaceAddresses(
	_ context.Context, req *netcheckv1.InterfaceAddressesRequest,
) (*netcheckv1.InterfaceAddressesResponse, error) {
	sel := netcheck.Selector{Name: req.GetName(), PCIAddress: req.GetPciAddress()}
	if err := sel.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	report, err := s.read(sel)
	if err != nil {
		return nil, class.Status(err)
	}
	return &netcheckv1.InterfaceAddressesResponse{Found: report.Found, Addresses: report.Addresses}, nil
}

// Connect opens a TCP connection from this node's host. A connection that does
// not succeed is an answer, not a failure.
func (s *Server) Connect(ctx context.Context, req *netcheckv1.ConnectRequest) (*netcheckv1.ConnectResponse, error) {
	address, port, err := endpoint(req.GetAddress(), int(req.GetPort()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	timeout := time.Duration(req.GetTimeoutSeconds()) * time.Second
	switch {
	case timeout <= 0:
		timeout = defaultTimeout
	case timeout > maxTimeout:
		timeout = maxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := s.connect(ctx, address, port); err != nil {
		return &netcheckv1.ConnectResponse{Connected: false, Detail: err.Error()}, nil
	}
	return &netcheckv1.ConnectResponse{Connected: true}, nil
}

// endpoint validates the address and port of a connection request.
func endpoint(address string, port int) (string, int, error) {
	if _, err := netip.ParseAddr(address); err != nil {
		return "", 0, fmt.Errorf("netcheckrpc: %q is not an IP address", address)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("netcheckrpc: port %d is out of range", port)
	}
	return address, port, nil
}

// Client reaches one node's CheckService.
type Client struct {
	client netcheckv1.CheckServiceClient
}

// Remote returns a Client over an established link connection.
func Remote(conn grpc.ClientConnInterface) *Client {
	return &Client{client: netcheckv1.NewCheckServiceClient(conn)}
}

// Result is the outcome of one connection attempt.
type Result struct {
	// Connected is whether the connection succeeded.
	Connected bool
	// Detail says why it did not, and is empty when it did.
	Detail string
}

// InterfaceAddresses reads one interface of the far node's host. An invalid
// selector is refused before anything is sent.
func (c *Client) InterfaceAddresses(ctx context.Context, sel netcheck.Selector) (netcheck.Report, error) {
	if err := sel.Validate(); err != nil {
		return netcheck.Report{}, err
	}
	resp, err := c.client.InterfaceAddresses(ctx, &netcheckv1.InterfaceAddressesRequest{
		Name: sel.Name, PciAddress: sel.PCIAddress,
	})
	if err != nil {
		return netcheck.Report{}, fmt.Errorf("node: read interface %q: %w", sel.Name+sel.PCIAddress, class.FromStatus(err))
	}
	return netcheck.Report{Found: resp.GetFound(), Addresses: resp.GetAddresses()}, nil
}

// Connect asks the far node's host to open a TCP connection to an address and
// port within the timeout. An invalid endpoint is refused before anything is sent.
func (c *Client) Connect(ctx context.Context, address string, port int, timeout time.Duration) (Result, error) {
	if _, _, err := endpoint(address, port); err != nil {
		return Result{}, err
	}
	resp, err := c.client.Connect(ctx, &netcheckv1.ConnectRequest{
		Address: address, Port: uint32(port), TimeoutSeconds: uint32(math.Ceil(timeout.Seconds())),
	})
	if err != nil {
		return Result{}, fmt.Errorf("node: connect to %s:%d: %w", address, port, class.FromStatus(err))
	}
	return Result{Connected: resp.GetConnected(), Detail: resp.GetDetail()}, nil
}
