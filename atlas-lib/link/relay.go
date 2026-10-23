package link

// The site link: how a peer on one site of a stretched cluster opens a stream
// to a peer on another (design-stretched-sync-replication-ramen.md §7).
//
// Nothing on a site listens for the other site, and the sites have no route to
// each other that anyone promises. What both have is an outbound connection to
// the hub. So a site runs a forwarder, which is a hub to its own peers and an
// agent of the relay on the hub, and a stream from site a to site b takes four
// hops, each an ordinary call of RelayService.Open on a link session that
// already exists:
//
//	caller --> a's forwarder --> relay --> b's forwarder --> target
//
// Every hop splices the call it serves to the one it makes. The caller runs an
// ordinary gRPC connection over the spliced bytes against whatever the target
// serves, so no hop parses anything after the first frame.
//
// Identity is decided once per hop by what that hop can verify. A forwarder
// knows its local caller from the caller's link token and stamps the stream's
// source; the relay knows the forwarder from its own link token, and refuses a
// source on any site but that forwarder's. A compromised forwarder can therefore
// speak for the peers of its own site and no other.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/link/sitev1"
)

// chunkSize bounds one data frame, well under gRPC's default message limit.
const chunkSize = 32 << 10

// remoteKinds are the kinds of local peer that may open a stream to another
// site. A node plugin is not one: what crosses sites is the operators' and the
// controllers' business, and a node that could reach the other site's operator
// directly would bypass its own.
var remoteKinds = map[PeerKind]bool{PeerKindOperator: true, PeerKindController: true}

// Relay is the hub end of the site link. Each site's forwarder links to the hub
// it is registered on as [SitePeer], and the relay delivers a stream from one
// site to the forwarder of the target's site.
type Relay struct {
	sitev1.UnimplementedRelayServiceServer

	registry *Registry
	log      *slog.Logger
}

// NewRelay returns a relay over the registry of the hub its forwarders link to.
// Register it on that hub's sessions with HubConfig.Register.
func NewRelay(registry *Registry, logger *slog.Logger) *Relay {
	if logger == nil {
		logger = slog.Default()
	}
	return &Relay{registry: registry, log: logger}
}

// Register adds the relay's service to a hub session.
func (r *Relay) Register(srv grpc.ServiceRegistrar) { sitev1.RegisterRelayServiceServer(srv, r) }

// Open delivers a stream a forwarder opened to the forwarder of the target's site.
func (r *Relay) Open(in sitev1.RelayService_OpenServer) error {
	caller, ok := PeerFromContext(in.Context())
	if !ok || caller.Kind != PeerKindSite {
		return status.Errorf(codes.PermissionDenied, "relay: %s is not a site forwarder", caller)
	}
	ends, err := firstEnds(in)
	if err != nil {
		return err
	}
	source, err := ParseRemote(ends.GetSource())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "relay: source: %v", err)
	}
	if source.Site != caller.Name {
		r.log.Warn("link: relay refused a source on another site", "forwarder", caller, "source", source)
		return status.Errorf(codes.PermissionDenied,
			"relay: the forwarder of site %s cannot open a stream from %s", caller.Name, source)
	}
	if !remoteKinds[source.Peer.Kind] {
		return status.Errorf(codes.PermissionDenied, "relay: a %s peer cannot open a stream to another site",
			source.Peer.Kind)
	}
	target, err := ParseRemote(ends.GetTarget())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "relay: target: %v", err)
	}
	site, err := r.registry.Peer(SitePeer(target.Site))
	if err != nil {
		return status.Errorf(codes.Unavailable, "relay: site %s is not linked", target.Site)
	}
	return splice(in, site.Conn(), ends)
}

// ForwarderConfig configures a [Forwarder].
type ForwarderConfig struct {
	// Site is the site this forwarder speaks for: the name it links to the
	// relay as, and the site of every source it stamps.
	Site string
	// Logger receives refusals. Nil means slog.Default.
	Logger *slog.Logger
}

// Forwarder is a site's end of the site link. It serves RelayService twice: to
// its own site's peers, through the local hub they link to, for the streams
// they open; and to the relay, through its own link to it, for the streams
// addressed to its site.
//
// It is built before the hub and agent it sits between, because each of them
// takes its services at construction: create the forwarder, pass RegisterLocal
// to the local hub and RegisterUpstream to the agent, and hand both back with
// SetLocal and SetUpstream before either serves.
type Forwarder struct {
	cfg ForwarderConfig
	log *slog.Logger

	mu       sync.RWMutex
	local    *Registry
	upstream *Agent
}

// NewForwarder returns a forwarder for one site.
func NewForwarder(cfg ForwarderConfig) *Forwarder {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Forwarder{cfg: cfg, log: cfg.Logger}
}

// SetLocal gives the forwarder the registry of its site's linked peers.
func (f *Forwarder) SetLocal(registry *Registry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.local = registry
}

// SetUpstream gives the forwarder its link to the relay.
func (f *Forwarder) SetUpstream(agent *Agent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upstream = agent
}

// RegisterLocal adds the service local peers open their streams through.
func (f *Forwarder) RegisterLocal(srv grpc.ServiceRegistrar) {
	sitev1.RegisterRelayServiceServer(srv, &forwarderOut{f: f})
}

// RegisterUpstream adds the service the relay delivers this site's streams to.
func (f *Forwarder) RegisterUpstream(srv grpc.ServiceRegistrar) {
	sitev1.RegisterRelayServiceServer(srv, &forwarderIn{f: f})
}

// forwarderOut serves the forwarder's own site's peers.
type forwarderOut struct {
	sitev1.UnimplementedRelayServiceServer
	f *Forwarder
}

// Open stamps the caller's verified identity as the stream's source and sends
// the stream on: to the relay for another site, to a local peer for this one.
func (o *forwarderOut) Open(in sitev1.RelayService_OpenServer) error {
	f := o.f
	caller, ok := PeerFromContext(in.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "forwarder: the caller is not an identified peer")
	}
	if !remoteKinds[caller.Kind] {
		f.log.Warn("link: forwarder refused a stream from a peer that may not open one", "peer", caller)
		return status.Errorf(codes.PermissionDenied, "forwarder: a %s peer cannot open a stream to another site",
			caller.Kind)
	}
	ends, err := firstEnds(in)
	if err != nil {
		return err
	}
	target, err := ParseRemote(ends.GetTarget())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "forwarder: target: %v", err)
	}
	stamped := &sitev1.StreamEnds{Target: target.String(), Source: Remote(f.cfg.Site, caller.Kind, caller.Name).String()}
	if target.Site == f.cfg.Site {
		return f.deliver(in, target, stamped)
	}

	f.mu.RLock()
	upstream := f.upstream
	f.mu.RUnlock()
	if upstream == nil {
		return status.Error(codes.Unavailable, "forwarder: not linked to the relay")
	}
	conn, err := upstream.Conn()
	if err != nil {
		return status.Error(codes.Unavailable, "forwarder: not linked to the relay")
	}
	return splice(in, conn, stamped)
}

// forwarderIn serves the relay.
type forwarderIn struct {
	sitev1.UnimplementedRelayServiceServer
	f *Forwarder
}

// Open delivers a stream the relay brought to this site's target peer.
func (i *forwarderIn) Open(in sitev1.RelayService_OpenServer) error {
	ends, err := firstEnds(in)
	if err != nil {
		return err
	}
	target, err := ParseRemote(ends.GetTarget())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "forwarder: target: %v", err)
	}
	if target.Site != i.f.cfg.Site {
		return status.Errorf(codes.InvalidArgument, "forwarder: %s is not on site %s", target, i.f.cfg.Site)
	}
	return i.f.deliver(in, target, ends)
}

// deliver splices a stream to the local peer target names.
func (f *Forwarder) deliver(in sitev1.RelayService_OpenServer, target RemotePeer, ends *sitev1.StreamEnds) error {
	f.mu.RLock()
	local := f.local
	f.mu.RUnlock()
	if local == nil {
		return status.Error(codes.Unavailable, "forwarder: no local peers")
	}
	var peer *Peer
	if target.Peer.Name != "" {
		peer, _ = local.Peer(target.Peer)
	} else if peers := local.PeersOfKind(target.Peer.Kind); len(peers) > 0 {
		peer = peers[0]
	}
	if peer == nil {
		return status.Errorf(codes.Unavailable, "forwarder: %s is not linked", target)
	}
	return splice(in, peer.Conn(), ends)
}

// Acceptor accepts the streams relayed to the peer it is registered on, as a
// [net.Listener] that peer runs a gRPC server on:
//
//	acceptor := link.NewAcceptor()
//	go grpcServer.Serve(acceptor)
//	link.NewAgent(link.AgentConfig{..., Register: acceptor.Register})
type Acceptor struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

// NewAcceptor returns an acceptor.
func NewAcceptor() *Acceptor {
	return &Acceptor{conns: make(chan net.Conn), done: make(chan struct{})}
}

// Register adds the service the forwarder delivers this peer's streams to.
func (a *Acceptor) Register(srv grpc.ServiceRegistrar) {
	sitev1.RegisterRelayServiceServer(srv, &acceptorService{a: a})
}

// Accept waits for the next relayed stream.
func (a *Acceptor) Accept() (net.Conn, error) {
	select {
	case c := <-a.conns:
		return c, nil
	case <-a.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting. Streams already accepted are unaffected.
func (a *Acceptor) Close() error {
	a.once.Do(func() { close(a.done) })
	return nil
}

// Addr names the acceptor; a relayed stream has no network address.
func (a *Acceptor) Addr() net.Addr { return remoteAddr("site-link") }

type acceptorService struct {
	sitev1.UnimplementedRelayServiceServer
	a *Acceptor
}

// Open hands a relayed stream to whoever is accepting, and holds the call open
// for as long as the stream lives.
func (s *acceptorService) Open(in sitev1.RelayService_OpenServer) error {
	ends, err := firstEnds(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(in.Context())
	defer cancel()
	c := newStreamConn(ctx, cancel, serverFrames{in}, remoteAddr(ends.GetSource()))
	select {
	case <-s.a.done:
		return status.Error(codes.Unavailable, "acceptor: not accepting")
	default:
	}
	// The empty frame is the acceptance every hop passes back to the caller,
	// which is what tells it the stream reached a peer rather than a refusal.
	if err := in.Send(&sitev1.OpenResponse{}); err != nil {
		return err
	}
	select {
	case s.a.conns <- c:
	case <-s.a.done:
		return status.Error(codes.Unavailable, "acceptor: not accepting")
	case <-ctx.Done():
		return nil
	}
	<-ctx.Done()
	return nil
}

// OpenRemote opens a byte stream to target through the forwarder agent is
// linked to. It returns once a peer accepted the stream, or the status the
// first hop that refused it answered with: Unavailable for a target that is not
// linked, which is retryable as [ErrNoSession] is.
func OpenRemote(ctx context.Context, agent *Agent, target RemotePeer) (net.Conn, error) {
	conn, err := agent.Conn()
	if err != nil {
		return nil, status.Error(codes.Unavailable, "link: not linked to the site's forwarder")
	}
	// The stream outlives ctx, which only bounds opening it: gRPC dials with a
	// context it cancels once the connection is up.
	streamCtx, cancel := context.WithCancel(context.Background())
	stream, err := sitev1.NewRelayServiceClient(conn).Open(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := stream.Send(&sitev1.OpenRequest{Kind: &sitev1.OpenRequest_Ends{
		Ends: &sitev1.StreamEnds{Target: target.String()}}}); err != nil {
		cancel()
		return nil, err
	}
	accepted := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		accepted <- err
	}()
	select {
	case err := <-accepted:
		if err != nil {
			cancel()
			return nil, err
		}
	case <-ctx.Done():
		cancel()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return newStreamConn(streamCtx, cancel, clientFrames{stream}, remoteAddr(target.String())), nil
}

// RemoteConn is a gRPC client of every service target serves to remote callers,
// reached through the forwarder agent is linked to. Each connection gRPC makes
// is a stream opened with [OpenRemote].
func RemoteConn(agent *Agent, target RemotePeer) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///"+dialTarget(target.String()),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return OpenRemote(ctx, agent, target)
		}),
	)
}

// firstEnds reads a stream's first frame, which must name its ends.
func firstEnds(in sitev1.RelayService_OpenServer) (*sitev1.StreamEnds, error) {
	first, err := in.Recv()
	if err != nil {
		return nil, err
	}
	ends := first.GetEnds()
	if ends == nil {
		return nil, status.Error(codes.InvalidArgument, "link: a relay stream must open with its ends")
	}
	return ends, nil
}

// splice opens the next hop on next with ends as its first frame and joins the
// two calls until either ends. The next hop's acceptance, or its refusal, is the
// first thing returned to the caller.
func splice(in sitev1.RelayService_OpenServer, next grpc.ClientConnInterface, ends *sitev1.StreamEnds) error {
	ctx, cancel := context.WithCancel(in.Context())
	defer cancel()

	out, err := sitev1.NewRelayServiceClient(next).Open(ctx)
	if err != nil {
		return err
	}
	if err := out.Send(&sitev1.OpenRequest{Kind: &sitev1.OpenRequest_Ends{Ends: ends}}); err != nil {
		return err
	}
	accepted, err := out.Recv()
	if err != nil {
		return err
	}
	if err := in.Send(accepted); err != nil {
		return err
	}

	go func() {
		for {
			frame, err := in.Recv()
			if errors.Is(err, io.EOF) {
				_ = out.CloseSend()
				return
			}
			if err != nil {
				cancel()
				return
			}
			if err := out.Send(frame); err != nil {
				cancel()
				return
			}
		}
	}()
	for {
		frame, err := out.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := in.Send(frame); err != nil {
			return err
		}
	}
}

// frames is one end of a relay stream, as the byte stream a streamConn needs.
type frames interface {
	send(data []byte) error
	recv() ([]byte, error)
	closeSend() error
}

type clientFrames struct {
	s sitev1.RelayService_OpenClient
}

func (c clientFrames) send(data []byte) error {
	return c.s.Send(&sitev1.OpenRequest{Kind: &sitev1.OpenRequest_Data{Data: data}})
}

func (c clientFrames) recv() ([]byte, error) {
	resp, err := c.s.Recv()
	return resp.GetData(), err
}

func (c clientFrames) closeSend() error { return c.s.CloseSend() }

type serverFrames struct {
	s sitev1.RelayService_OpenServer
}

func (s serverFrames) send(data []byte) error { return s.s.Send(&sitev1.OpenResponse{Data: data}) }

func (s serverFrames) recv() ([]byte, error) {
	req, err := s.s.Recv()
	return req.GetData(), err
}

// closeSend is a no-op: a server ends its half by returning from the call,
// which the streamConn's cancel does.
func (s serverFrames) closeSend() error { return nil }

// streamConn is a relay stream as a net.Conn.
type streamConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	frames frames
	remote net.Addr

	rmu  sync.Mutex
	rbuf []byte
	wmu  sync.Mutex
	once sync.Once
}

func newStreamConn(ctx context.Context, cancel context.CancelFunc, f frames, remote net.Addr) *streamConn {
	return &streamConn{ctx: ctx, cancel: cancel, frames: f, remote: remote}
}

func (c *streamConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.rbuf) == 0 {
		data, err := c.frames.recv()
		if err != nil {
			if c.ctx.Err() != nil && !errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("link: relay stream closed: %w", err)
			}
			return 0, err
		}
		c.rbuf = data
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *streamConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0
	for written < len(p) {
		end := min(written+chunkSize, len(p))
		// The frame is sent after Write returns the caller's buffer to it, so
		// it carries a copy rather than the caller's slice.
		if err := c.frames.send(append([]byte(nil), p[written:end]...)); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

func (c *streamConn) Close() error {
	c.once.Do(func() {
		c.wmu.Lock()
		_ = c.frames.closeSend()
		c.wmu.Unlock()
		c.cancel()
	})
	return nil
}

func (c *streamConn) LocalAddr() net.Addr  { return remoteAddr("site-link") }
func (c *streamConn) RemoteAddr() net.Addr { return c.remote }

// Deadlines are not supported on a relayed stream; gRPC, its only user, bounds
// its calls with contexts instead.
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

// remoteAddr is a site-link address, the peer written as site/<site>/<kind>/<name>.
type remoteAddr string

func (a remoteAddr) Network() string { return "site-link" }
func (a remoteAddr) String() string  { return string(a) }
