// Package swaprpc moves a staged volume's namespace to another NVMe-oF
// subsystem under a live consumer, across a link: the CSI node plugin serves
// SwapService over its Swapper, and the operator calls it while the control
// plane moves the namespace (design-cg-live-subsystem-flip.md §6).
//
// On the node:
//
//	sw, err := swaprpc.New(swaprpc.Config{Records: store, Mapper: devmapper.New(nil), ...})
//	err = sw.Recover(ctx) // before serving: nothing left suspended by a previous process
//	cfg.Register = func(r grpc.ServiceRegistrar) { swaprpc.NewServer(sw).Register(r) }
//	cfg.Capabilities = append(cfg.Capabilities, swaprpc.Capabilities()...)
//
// From the operator, per node peer that advertises CapabilitySwap:
//
//	states, err := swaprpc.Remote(peer.Conn()).Suspend(ctx, handles, deadline)
package swaprpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/nvmeof"
	"github.com/simplyblock/atlas/volstack/swaprpc/swapv1"
)

// CapabilitySwap is what a node advertises when it serves SwapService.
const CapabilitySwap = "volume-swap"

// Capabilities names what this package serves, for the link handshake.
func Capabilities() []string { return []string{CapabilitySwap} }

// Server serves SwapService over a Swapper.
type Server struct {
	swapv1.UnimplementedSwapServiceServer
	swapper *Swapper
}

// NewServer serves sw.
func NewServer(sw *Swapper) *Server { return &Server{swapper: sw} }

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) { swapv1.RegisterSwapServiceServer(r, s) }

// Describe reports the named volumes.
func (s *Server) Describe(ctx context.Context, req *swapv1.DescribeRequest) (*swapv1.DescribeResponse, error) {
	states, err := s.swapper.Describe(ctx, req.GetVolumeHandles())
	if err != nil {
		return nil, toStatus(err)
	}
	return &swapv1.DescribeResponse{Volumes: toWire(states)}, nil
}

// Prepare connects the target subsystem's paths.
func (s *Server) Prepare(ctx context.Context, req *swapv1.PrepareRequest) (*swapv1.PrepareResponse, error) {
	if req.GetTargetNqn() == "" || len(req.GetPaths()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "swap: a target NQN and at least one path are required")
	}
	paths := make([]nvmeof.Target, 0, len(req.GetPaths()))
	for _, p := range req.GetPaths() {
		paths = append(paths, fromWirePath(p))
	}
	states, err := s.swapper.Prepare(ctx, req.GetVolumeHandles(), req.GetTargetNqn(), paths)
	if err != nil {
		return nil, toStatus(err)
	}
	return &swapv1.PrepareResponse{Volumes: toWire(states)}, nil
}

// Suspend holds the named volumes' I/O until the deadline.
func (s *Server) Suspend(ctx context.Context, req *swapv1.SuspendRequest) (*swapv1.SuspendResponse, error) {
	states, err := s.swapper.Suspend(ctx, req.GetVolumeHandles(), time.Unix(req.GetDeadlineUnixSeconds(), 0))
	if err != nil {
		return nil, toStatus(err)
	}
	return &swapv1.SuspendResponse{Volumes: toWire(states)}, nil
}

// Resume re-points the named volumes at the subsystem and releases them.
func (s *Server) Resume(ctx context.Context, req *swapv1.ResumeRequest) (*swapv1.ResumeResponse, error) {
	if req.GetNqn() == "" {
		return nil, status.Error(codes.InvalidArgument, "swap: the subsystem to resume on is required")
	}
	states, err := s.swapper.Resume(ctx, req.GetVolumeHandles(), req.GetNqn())
	if err != nil {
		return nil, toStatus(err)
	}
	return &swapv1.ResumeResponse{Volumes: toWire(states)}, nil
}

// Release disconnects a subsystem nothing staged here uses.
func (s *Server) Release(ctx context.Context, req *swapv1.ReleaseRequest) (*swapv1.ReleaseResponse, error) {
	if req.GetNqn() == "" {
		return nil, status.Error(codes.InvalidArgument, "swap: the subsystem to release is required")
	}
	done, err := s.swapper.Release(ctx, req.GetNqn())
	if err != nil {
		return nil, toStatus(err)
	}
	return &swapv1.ReleaseResponse{Disconnected: done}, nil
}

// Each refusal travels as its own code, so the client restores the sentinel
// and the operator can choose between failing, rolling back, and retrying.
var refusals = []struct {
	err  error
	code codes.Code
}{
	{ErrNotSwappable, codes.FailedPrecondition},
	{ErrDeadlinePassed, codes.OutOfRange},
	{ErrNamespaceNotFound, codes.NotFound},
}

func toStatus(err error) error {
	for _, r := range refusals {
		if errors.Is(err, r.err) {
			return status.Error(r.code, err.Error())
		}
	}
	return status.Error(codes.Unavailable, err.Error())
}

func fromStatus(op string, err error) error {
	st, ok := status.FromError(err)
	if ok {
		for _, r := range refusals {
			if st.Code() == r.code {
				return refused{msg: "node: " + op + ": " + st.Message(), err: r.err}
			}
		}
	}
	return fmt.Errorf("node: %s: %w", op, err)
}

// refused is a refusal restored on the caller's side: it reads as the node's
// message and matches the sentinel the node refused with.
type refused struct {
	msg string
	err error
}

func (r refused) Error() string { return r.msg }
func (r refused) Unwrap() error { return r.err }

func toWire(states []VolumeState) []*swapv1.VolumeState {
	out := make([]*swapv1.VolumeState, 0, len(states))
	for _, s := range states {
		out = append(out, &swapv1.VolumeState{
			VolumeHandle:  s.Handle,
			Staged:        s.Staged,
			Swappable:     s.Swappable,
			Suspended:     s.Suspended,
			Nqn:           s.NQN,
			NamespaceUuid: s.NamespaceUUID,
		})
	}
	return out
}

func fromWire(states []*swapv1.VolumeState) []VolumeState {
	out := make([]VolumeState, 0, len(states))
	for _, s := range states {
		out = append(out, VolumeState{
			Handle:        s.GetVolumeHandle(),
			Staged:        s.GetStaged(),
			Swappable:     s.GetSwappable(),
			Suspended:     s.GetSuspended(),
			NQN:           s.GetNqn(),
			NamespaceUUID: s.GetNamespaceUuid(),
		})
	}
	return out
}

func toWirePath(t nvmeof.Target) *swapv1.Path {
	p := &swapv1.Path{
		Transport:               string(t.Transport),
		Address:                 t.Address,
		Port:                    int32(t.Port), //nolint:gosec // a TCP port
		HostIface:               t.HostIface,
		NrIoQueues:              int32(t.NrIOQueues),        //nolint:gosec // a queue count
		ReconnectDelaySeconds:   int32(t.ReconnectDelaySec), //nolint:gosec // seconds
		KeepAliveTimeoutSeconds: int32(t.KeepAliveTMOSec),   //nolint:gosec // seconds
	}
	if t.CtrlLossTMOSec != nil {
		v := int32(*t.CtrlLossTMOSec) //nolint:gosec // seconds
		p.CtrlLossTimeoutSeconds = &v
	}
	if t.FastIOFailTMOSec != nil {
		v := int32(*t.FastIOFailTMOSec) //nolint:gosec // seconds
		p.FastIoFailTimeoutSeconds = &v
	}
	return p
}

func fromWirePath(p *swapv1.Path) nvmeof.Target {
	t := nvmeof.Target{
		Transport:         nvmeof.Transport(p.GetTransport()),
		Address:           p.GetAddress(),
		Port:              int(p.GetPort()),
		HostIface:         p.GetHostIface(),
		NrIOQueues:        int(p.GetNrIoQueues()),
		ReconnectDelaySec: int(p.GetReconnectDelaySeconds()),
		KeepAliveTMOSec:   int(p.GetKeepAliveTimeoutSeconds()),
	}
	if p.CtrlLossTimeoutSeconds != nil {
		v := int(p.GetCtrlLossTimeoutSeconds())
		t.CtrlLossTMOSec = &v
	}
	if p.FastIoFailTimeoutSeconds != nil {
		v := int(p.GetFastIoFailTimeoutSeconds())
		t.FastIOFailTMOSec = &v
	}
	return t
}

// Client reaches one node's SwapService.
type Client struct {
	client swapv1.SwapServiceClient
}

// Remote returns a Client over an established link connection.
func Remote(conn grpc.ClientConnInterface) *Client {
	return &Client{client: swapv1.NewSwapServiceClient(conn)}
}

// Describe reports the named volumes, in the order asked.
func (c *Client) Describe(ctx context.Context, handles []string) ([]VolumeState, error) {
	resp, err := c.client.Describe(ctx, &swapv1.DescribeRequest{VolumeHandles: handles})
	if err != nil {
		return nil, fromStatus("describe", err)
	}
	return fromWire(resp.GetVolumes()), nil
}

// Prepare connects the node to targetNQN over paths, in the order given. The
// paths' NQN and host identity are the node's to fill in.
func (c *Client) Prepare(
	ctx context.Context, handles []string, targetNQN string, paths []nvmeof.Target,
) ([]VolumeState, error) {
	wire := make([]*swapv1.Path, 0, len(paths))
	for _, p := range paths {
		wire = append(wire, toWirePath(p))
	}
	resp, err := c.client.Prepare(ctx, &swapv1.PrepareRequest{VolumeHandles: handles, TargetNqn: targetNQN, Paths: wire})
	if err != nil {
		return nil, fromStatus("prepare "+targetNQN, err)
	}
	return fromWire(resp.GetVolumes()), nil
}

// Suspend holds the named volumes' I/O until Resume or deadline. A volume that
// cannot swap fails the whole call with ErrNotSwappable, and a deadline
// already passed with ErrDeadlinePassed, and in both cases nothing is held.
func (c *Client) Suspend(ctx context.Context, handles []string, deadline time.Time) ([]VolumeState, error) {
	resp, err := c.client.Suspend(ctx, &swapv1.SuspendRequest{VolumeHandles: handles, DeadlineUnixSeconds: deadline.Unix()})
	if err != nil {
		return nil, fromStatus("suspend", err)
	}
	return fromWire(resp.GetVolumes()), nil
}

// Resume points the named volumes at their namespaces under nqn and releases
// them. ErrNamespaceNotFound means a namespace never appeared there and its
// volume is still held.
func (c *Client) Resume(ctx context.Context, handles []string, nqn string) ([]VolumeState, error) {
	resp, err := c.client.Resume(ctx, &swapv1.ResumeRequest{VolumeHandles: handles, Nqn: nqn})
	if err != nil {
		return nil, fromStatus("resume on "+nqn, err)
	}
	return fromWire(resp.GetVolumes()), nil
}

// Release disconnects nqn on the node, and reports false when a staged volume
// still points at it.
func (c *Client) Release(ctx context.Context, nqn string) (bool, error) {
	resp, err := c.client.Release(ctx, &swapv1.ReleaseRequest{Nqn: nqn})
	if err != nil {
		return false, fromStatus("release "+nqn, err)
	}
	return resp.GetDisconnected(), nil
}
