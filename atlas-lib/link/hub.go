package link

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/internal/version"
	"github.com/simplyblock/atlas/link/linkv1"
)

// defaultHandshakeTimeout bounds how long an unidentified session may sit on
// the hub. It is generous on purpose: the handshake includes a TokenReview
// against the API server, which is slow exactly when the cluster is unwell.
const defaultHandshakeTimeout = 30 * time.Second

// HubConfig configures a [Hub]. Listener and Auth are required.
type HubConfig struct {
	// Listener accepts peer connections. The hub takes ownership of it and
	// closes it on shutdown.
	//
	// It must be encrypted — wrap it with tls.NewListener. Peers authenticate
	// with bearer tokens, and a bearer token on a plaintext connection is a
	// credential handed to anyone on the path.
	Listener net.Listener

	// Auth decides who each peer is. Use [KubeAuthenticator] in a cluster.
	Auth Authenticator

	// Accepting gates whether the hub takes on new peers. Nil always accepts.
	//
	// A link terminates on exactly one operator replica, and the one that must
	// hold it is the one doing the reconciling. Wire this to leadership so a
	// replica that is not the leader turns peers away and they redial to the
	// one that is:
	//
	//	Accepting: func() bool {
	//	    select {
	//	    case <-mgr.Elected():
	//	        return true
	//	    default:
	//	        return false
	//	    }
	//	}
	//
	// Running the hub as a leader-election Runnable is the primary control —
	// this is the belt-and-braces for the window where the listener is up but
	// leadership is not yet settled, and for draining without a restart.
	Accepting func() bool

	// HandshakeTimeout bounds how long a session may stay unidentified before
	// the hub closes it. Zero means 30s.
	HandshakeTimeout time.Duration

	// Register adds the services the hub serves *back* to its peers, for peers
	// that call the operator as well as answer it. Nil registers none, and the
	// link still works in the direction that matters.
	Register func(grpc.ServiceRegistrar)

	// Capabilities names those services in the Hello answer.
	Capabilities []string

	// Version identifies the hub build to its peers. Empty means the atlas
	// build version.
	Version string

	// Yamux tunes the multiplexer. Nil means [DefaultYamuxConfig].
	Yamux *yamux.Config

	// Logger receives session lifecycle events. Nil means slog.Default.
	Logger *slog.Logger
}

// Hub accepts peer links and keeps a [Registry] of who is reachable.
//
// It is the operator's end of the link. Peers dial it, identify themselves, and
// are registered for as long as their session lasts; the operator then issues
// RPCs down those sessions through the registry.
type Hub struct {
	cfg      HubConfig
	log      *slog.Logger
	registry *Registry

	mu       sync.Mutex
	sessions map[*Session]struct{}
	closed   bool
}

// NewHub returns a hub ready to [Hub.Serve]. It does not accept anything until
// Serve runs.
func NewHub(cfg HubConfig) (*Hub, error) {
	if cfg.Listener == nil {
		return nil, fmt.Errorf("link hub: no listener: %w", errs.ErrUnsupported)
	}
	if cfg.Auth == nil {
		return nil, fmt.Errorf("link hub: no authenticator: %w", errs.ErrUnsupported)
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = defaultHandshakeTimeout
	}
	if cfg.Version == "" {
		cfg.Version = version.Version
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Hub{
		cfg:      cfg,
		log:      cfg.Logger,
		registry: NewRegistry(),
		sessions: make(map[*Session]struct{}),
	}, nil
}

// Registry is the set of peers currently linked.
func (h *Hub) Registry() *Registry { return h.registry }

// Addr is the address the hub listens on.
func (h *Hub) Addr() net.Addr { return h.cfg.Listener.Addr() }

// Serve accepts peer links until ctx ends or the listener fails, then shuts the
// hub down: the listener closes and every session with it, so peers notice
// immediately and redial rather than holding a connection to a hub that has
// stopped answering. It returns ctx's error on a clean shutdown.
func (h *Hub) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Accept does not take a context; closing the listener is what unblocks it.
	go func() {
		<-ctx.Done()
		_ = h.Close()
	}()

	h.log.Info("link: hub accepting peers", "addr", h.Addr())
	for {
		raw, err := h.cfg.Listener.Accept()
		if err != nil {
			if h.isClosed() {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return nil
			}
			return fmt.Errorf("link hub: accept: %w", err)
		}
		go h.handle(raw)
	}
}

// Close shuts the hub down: the listener stops accepting and every live session
// is torn down. It is idempotent.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	sessions := make([]*Session, 0, len(h.sessions))
	for s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	err := h.cfg.Listener.Close()
	for _, s := range sessions {
		_ = s.Close()
	}
	return err
}

// handle runs one accepted connection for its whole life: multiplex it, serve
// the handshake on it, and wait for it to end.
func (h *Hub) handle(raw net.Conn) {
	remote := raw.RemoteAddr()
	hs := &handshake{hub: h, ready: make(chan struct{})}

	sess, err := newSession(raw, false, sessionConfig{
		name:  remote.String(),
		yamux: h.cfg.Yamux,
		register: func(srv grpc.ServiceRegistrar) {
			linkv1.RegisterLinkServiceServer(srv, hs)
			if h.cfg.Register != nil {
				h.cfg.Register(srv)
			}
		},
	})
	if err != nil {
		h.log.Warn("link: could not establish session", "remote", remote, "error", err)
		return
	}
	// Publishing the session releases any Hello already in flight — the server
	// is serving from the moment it exists, so a fast peer can beat this line.
	hs.session = sess
	close(hs.ready)

	if !h.track(sess) {
		// The hub shut down between accept and here.
		_ = sess.Close()
		return
	}
	defer h.untrack(sess)

	timer := time.AfterFunc(h.cfg.HandshakeTimeout, func() {
		if _, ok := hs.identity(); !ok {
			h.log.Warn("link: peer never identified itself; closing session",
				"remote", remote, "timeout", h.cfg.HandshakeTimeout)
			_ = sess.Close()
		}
	})
	defer timer.Stop()

	<-sess.Done()

	if id, ok := hs.identity(); ok {
		h.registry.unregister(id, sess)
		h.log.Info("link: peer disconnected", "peer", id, "remote", remote)
	}
}

func (h *Hub) isAccepting() bool {
	if h.cfg.Accepting == nil {
		return true
	}
	return h.cfg.Accepting()
}

func (h *Hub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// track records a session so shutdown can reach it. It reports false when the
// hub is already closed, in which case the caller must drop the session.
func (h *Hub) track(s *Session) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.sessions[s] = struct{}{}
	return true
}

func (h *Hub) untrack(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, s)
}

// handshake serves [linkv1.LinkService] for exactly one session — it holds that
// session, which is how a call that arrives over a connection turns into a
// registration of the connection it arrived on.
//
// A rejected Hello does not close the session. The peer sees the error and
// tears down its own end, which is the fast path; the handshake timer covers a
// peer that does not. Closing it here instead would race the error response out
// of the connection it has to travel over.
type handshake struct {
	linkv1.UnimplementedLinkServiceServer

	hub *Hub

	// ready is closed once session is set, publishing it to handlers.
	ready   chan struct{}
	session *Session

	mu         sync.Mutex
	identified bool
	id         Identity
}

// Hello authenticates a peer and registers it.
func (hs *handshake) Hello(ctx context.Context, req *linkv1.HelloRequest) (*linkv1.HelloResponse, error) {
	h := hs.hub

	if !h.isAccepting() {
		// Not this replica's peer to hold. Unavailable is deliberate: the peer
		// retries, and the Service points the retry at whoever is leading.
		return nil, status.Error(codes.Unavailable, "link: hub is not accepting sessions")
	}

	sess, err := hs.awaitSession(ctx)
	if err != nil {
		return nil, err
	}

	token, err := bearerToken(ctx)
	if err != nil {
		return nil, err
	}

	claim := Claim{
		ID:           PeerID{Kind: PeerKind(req.GetKind()), Name: req.GetName()},
		InstanceUID:  req.GetInstanceUid(),
		Capabilities: req.GetCapabilities(),
		Version:      req.GetVersion(),
	}

	identity, err := h.cfg.Auth.Authenticate(ctx, token, claim)
	if err != nil {
		h.log.Warn("link: rejected peer",
			"remote", sess.RemoteAddr(), "claimed", claim.ID, "error", err)
		return nil, class.Status(err)
	}
	if err := identity.ID.validate(); err != nil {
		return nil, status.Errorf(codes.Internal, "link: authenticator returned an unusable identity: %v", err)
	}

	// Claim the session before touching the registry, so two Hellos racing on
	// one connection cannot both register it.
	if err := hs.complete(identity); err != nil {
		return nil, err
	}

	peer := &Peer{
		ID:           identity.ID,
		InstanceUID:  identity.InstanceUID,
		Capabilities: claim.Capabilities,
		Version:      claim.Version,
		ConnectedAt:  time.Now(),
		session:      sess,
	}
	if old := h.registry.register(peer); old != nil && old.session != sess {
		h.log.Info("link: superseding earlier session for peer",
			"peer", identity.ID, "previous", old.InstanceUID, "current", identity.InstanceUID)
		go func() { _ = old.Close() }()
	}

	h.log.Info("link: peer linked",
		"peer", identity.ID, "instance", identity.InstanceUID,
		"remote", sess.RemoteAddr(), "version", claim.Version, "capabilities", claim.Capabilities)

	return &linkv1.HelloResponse{
		Kind:         string(identity.ID.Kind),
		Name:         identity.ID.Name,
		Capabilities: h.cfg.Capabilities,
		Version:      h.cfg.Version,
	}, nil
}

// awaitSession blocks until the session this handshake belongs to is published.
func (hs *handshake) awaitSession(ctx context.Context) (*Session, error) {
	select {
	case <-hs.ready:
		return hs.session, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// complete marks the session identified, refusing a second Hello.
func (hs *handshake) complete(id Identity) error {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.identified {
		return status.Error(codes.FailedPrecondition, "link: session is already identified")
	}
	hs.identified, hs.id = true, id
	return nil
}

// identity is the verified identity of this session, and whether it has one.
func (hs *handshake) identity() (PeerID, bool) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.id.ID, hs.identified
}