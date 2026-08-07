package link

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Session is one link: a multiplexed connection carrying gRPC in both
// directions at once.
//
// Both ends hold the same thing. Each runs a grpc.Server over the streams the
// other opens and a grpc.ClientConn over the streams it opens itself, so which
// end dialled stops mattering the moment the session exists. That symmetry is
// the whole reason for the multiplexer: it is what lets the CSI driver dial out
// and still be the one answering calls.
type Session struct {
	mux *yamux.Session
	srv *grpc.Server
	cc  *grpc.ClientConn

	closeOnce sync.Once
	closeErr  error
}

// sessionConfig is the internal knobs newSession takes; Hub and Agent expose
// the ones a caller has any business setting.
type sessionConfig struct {
	// name labels the session's client connection for diagnostics only — the
	// dialer ignores the target and opens a stream on the multiplexer.
	name string
	// register adds the services this side serves to the session's server. It
	// runs before the server starts, which is when gRPC requires it.
	register func(grpc.ServiceRegistrar)
	// yamux tunes the multiplexer; nil means DefaultYamuxConfig.
	yamux *yamux.Config
}

// DefaultYamuxConfig is the multiplexer tuning a link uses unless told
// otherwise. It differs from yamux's own defaults in two places that matter for
// a long-lived control channel:
//
//   - StreamOpenTimeout drops from 75s to 15s. Opening a stream is what a gRPC
//     dial does, so the default makes a wedged session absorb calls for over a
//     minute before failing them. Shorter means the connection fails fast, the
//     hub drops the peer, and the peer reconnects. (yamux closes the session
//     when this fires, which is exactly what should happen.)
//   - Keepalive halves to 15s, so a peer that dies without closing its TCP
//     connection is noticed and deregistered in seconds rather than a minute.
//
// yamux's own logging is discarded: it writes to stderr by default, unlabelled
// and interleaved with everything else. The link reports session lifecycle
// through its own logger; set LogOutput or Logger on the returned config if you
// want the multiplexer's internals too.
func DefaultYamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.StreamOpenTimeout = 15 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}

// newSession multiplexes raw and brings gRPC up on it in both directions.
// client selects the multiplexer's side; the two ends must disagree, which they
// do by construction — the agent dials, the hub accepts.
//
// It takes ownership of raw: on failure raw is closed, and on success it lives
// until the session is closed.
func newSession(raw net.Conn, client bool, cfg sessionConfig) (*Session, error) {
	ycfg := cfg.yamux
	if ycfg == nil {
		ycfg = DefaultYamuxConfig()
	}

	var (
		mux *yamux.Session
		err error
	)
	if client {
		mux, err = yamux.Client(raw, ycfg)
	} else {
		mux, err = yamux.Server(raw, ycfg)
	}
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("link: multiplex %s: %w", raw.RemoteAddr(), err)
	}

	s := &Session{mux: mux, srv: grpc.NewServer()}
	if cfg.register != nil {
		cfg.register(s.srv)
	}

	// The transport under this connection is the multiplexer, whose own
	// connection is already TLS if the caller's dialer or listener made it so;
	// a second handshake inside it would encrypt what is already encrypted.
	// Nor is gRPC keepalive configured: yamux runs its own, and two independent
	// liveness probes on one connection only disagree about whether it is dead.
	cc, err := grpc.NewClient("passthrough:///"+dialTarget(cfg.name),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(s.dial),
	)
	if err != nil {
		_ = mux.Close()
		return nil, fmt.Errorf("link: client for %s: %w", raw.RemoteAddr(), err)
	}
	s.cc = cc

	go func() {
		// Serve returns when the multiplexer dies (the peer went away, a
		// keepalive failed) or when the session is closed deliberately. Either
		// way the session is over, so tear the rest of it down.
		_ = s.srv.Serve(mux)
		_ = s.Close()
	}()

	return s, nil
}

// Conn is a client of every service the *other* end registered.
func (s *Session) Conn() *grpc.ClientConn { return s.cc }

// Done is closed once the session has ended, for whatever reason.
func (s *Session) Done() <-chan struct{} { return s.mux.CloseChan() }

// RemoteAddr is the address of the other end, for diagnostics.
func (s *Session) RemoteAddr() net.Addr { return s.mux.RemoteAddr() }

// Close ends the session and releases everything under it. It is idempotent,
// and safe to call from inside an RPC handler running on this very session —
// which the handshake does when it rejects a peer.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		// Closing the multiplexer is what actually ends the session: Serve
		// returns and every stream in flight fails. Stopping the gRPC server is
		// housekeeping, and it blocks until in-flight handlers return — which
		// would include a handler that called Close — so it happens off to the
		// side rather than deadlocking the caller.
		s.closeErr = s.mux.Close()
		_ = s.cc.Close()
		go s.srv.Stop()
	})
	return s.closeErr
}

// dial opens a stream for gRPC to run one connection over. gRPC calls it again
// whenever it needs a fresh transport, so a session that has ended must fail
// here rather than block: the connection then goes to TRANSIENT_FAILURE and the
// owner (hub registry or agent loop) drops it.
func (s *Session) dial(ctx context.Context, _ string) (net.Conn, error) {
	if s.mux.IsClosed() {
		return nil, ErrNoSession
	}

	// yamux's Open takes no context, so honour the caller's deadline here
	// instead of inheriting the multiplexer's stream-open timeout.
	type opened struct {
		conn net.Conn
		err  error
	}
	ch := make(chan opened, 1)
	go func() {
		conn, err := s.mux.Open()
		ch <- opened{conn: conn, err: err}
	}()

	select {
	case o := <-ch:
		return o.conn, o.err
	case <-ctx.Done():
		// The open may still land after we stop waiting. Close what it
		// produced rather than leaving a stream open that nobody will read.
		go func() {
			if o := <-ch; o.conn != nil {
				_ = o.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// dialTarget makes a name safe to use as a passthrough gRPC target, which a
// peer id ("node/worker-3") is not. The value is cosmetic — it appears in
// connection diagnostics and nowhere else, since the dialer never reads it.
func dialTarget(name string) string {
	if name == "" {
		return "link"
	}
	return strings.ReplaceAll(name, "/", ".")
}