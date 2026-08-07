package link

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/errs"
	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/internal/version"
	"github.com/simplyblock/atlas/link/linkv1"
)

// Defaults for an agent's timing. The backoff ceiling is deliberately modest:
// the thing being waited for is usually an operator leadership change or a pod
// rollout, both of which finish in seconds, and a node that stays unlinked is a
// node the operator cannot see.
const (
	defaultDialTimeout  = 15 * time.Second
	defaultHelloTimeout = 30 * time.Second
	defaultMinBackoff   = 1 * time.Second
	defaultMaxBackoff   = 30 * time.Second
)

// Dialer opens one connection to the hub.
type Dialer func(ctx context.Context) (net.Conn, error)

// AgentConfig configures an [Agent]. Dial, ID and Token are required.
type AgentConfig struct {
	// Dial opens a connection to the hub. Use [TLSDialer]: the token this
	// agent presents is a bearer credential, so the connection under it has to
	// be encrypted.
	Dial Dialer

	// ID is the identity this peer asks to register as, e.g.
	// link.NodePeer(os.Getenv("NODE_NAME")). The hub verifies it against the
	// credentials and refuses the link if they disagree, so it is a claim, not
	// a choice — get it from the downward API rather than from configuration.
	ID PeerID

	// InstanceUID distinguishes this process lifetime from the previous one,
	// letting the hub supersede a session an earlier instance left behind. In
	// Kubernetes pass the pod UID (downward API metadata.uid). Empty generates
	// one, which works but tells the hub nothing it can verify.
	InstanceUID string

	// Token produces the credentials presented at each link attempt. Use
	// [TokenFile] so rotation is picked up.
	Token TokenSource

	// Register adds the services this peer serves to the hub. Nil serves none,
	// which makes the link useful only in the direction of the hub's own
	// services.
	Register func(grpc.ServiceRegistrar)

	// Capabilities names those services in the Hello, so the hub can tell what
	// this peer can do before it calls.
	Capabilities []string

	// Version identifies this build to the hub. Empty means the atlas build
	// version.
	Version string

	// DialTimeout bounds one dial. Zero means 15s.
	DialTimeout time.Duration
	// HelloTimeout bounds the handshake, which on the hub side includes a
	// TokenReview. Zero means 30s.
	HelloTimeout time.Duration
	// MinBackoff and MaxBackoff bound the wait between link attempts. Zero
	// means 1s and 30s. The actual wait is jittered, so a hub that drops every
	// peer at once does not get them all back in the same instant.
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// Yamux tunes the multiplexer. Nil means [DefaultYamuxConfig].
	Yamux *yamux.Config

	// Logger receives link lifecycle events. Nil means slog.Default.
	Logger *slog.Logger

	// OnLink runs when a session is established and identified, OnUnlink when
	// it ends. Both run on the agent's own goroutine, so neither should block
	// for long — a slow OnLink delays nothing but the wait for the session to
	// end, but a slow OnUnlink delays reconnection.
	OnLink   func(*Session)
	OnUnlink func()
}

// Agent is the CSI driver's end of the link: it dials the hub, identifies
// itself, serves its own services down the connection, and keeps doing so.
//
// One [Agent.Run] call is the whole lifecycle. A session that drops — the
// operator restarted, leadership moved, the network blinked — is not an error
// to report upward but a reason to reconnect, which is what Run does until its
// context ends.
type Agent struct {
	cfg AgentConfig
	log *slog.Logger

	mu       sync.RWMutex
	session  *Session
	identity PeerID
}

// NewAgent returns an agent ready to [Agent.Run].
func NewAgent(cfg AgentConfig) (*Agent, error) {
	if cfg.Dial == nil {
		return nil, fmt.Errorf("link agent: no dialer: %w", errs.ErrUnsupported)
	}
	if err := cfg.ID.validate(); err != nil {
		return nil, fmt.Errorf("link agent: %w", err)
	}
	if cfg.Token == nil {
		return nil, fmt.Errorf("link agent: no token source: %w", errs.ErrUnsupported)
	}
	if cfg.InstanceUID == "" {
		cfg.InstanceUID = uuid.NewString()
	}
	if cfg.Version == "" {
		cfg.Version = version.Version
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.HelloTimeout <= 0 {
		cfg.HelloTimeout = defaultHelloTimeout
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(defaultMaxBackoff, cfg.MinBackoff)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Agent{cfg: cfg, log: cfg.Logger}, nil
}

// Run links to the hub and keeps it linked until ctx ends, at which point it
// returns ctx's error. It never returns because a link attempt failed.
func (a *Agent) Run(ctx context.Context) error {
	backoff := a.cfg.MinBackoff
	for {
		linked, err := a.link(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		switch {
		case linked:
			// A session that came up and later ended says the hub is there and
			// reachable, so the next attempt starts from the bottom of the ramp
			// rather than inheriting a ceiling from some earlier outage.
			backoff = a.cfg.MinBackoff
			a.log.Info("link: session ended, reconnecting", "peer", a.cfg.ID)
		default:
			a.log.Warn("link: could not link to hub", "peer", a.cfg.ID, "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(backoff)):
		}

		if !linked {
			backoff = min(backoff*2, a.cfg.MaxBackoff)
		}
	}
}

// Conn is a client of the services the hub serves back to this peer. It fails
// with [ErrNoSession] while the agent is not linked.
func (a *Agent) Conn() (grpc.ClientConnInterface, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.session == nil {
		return nil, fmt.Errorf("hub: %w", ErrNoSession)
	}
	return a.session.Conn(), nil
}

// Linked reports whether a session is currently up and identified.
func (a *Agent) Linked() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.session != nil
}

// Identity is what the hub last said this peer is, which may differ from what
// it claimed — the hub's answer is the authoritative one. It is the zero PeerID
// before the first successful handshake.
func (a *Agent) Identity() PeerID {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.identity
}

// link runs one attempt for its whole life: dial, handshake, serve until the
// session ends. It reports whether the session was ever established, which is
// what decides whether the backoff ramp resets.
func (a *Agent) link(ctx context.Context) (bool, error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, a.cfg.DialTimeout)
	raw, err := a.cfg.Dial(dialCtx)
	cancelDial()
	if err != nil {
		return false, fmt.Errorf("link: dialing hub: %w", err)
	}

	sess, err := newSession(raw, true, sessionConfig{
		name:     a.cfg.ID.String(),
		yamux:    a.cfg.Yamux,
		register: a.cfg.Register,
	})
	if err != nil {
		return false, err
	}
	defer func() { _ = sess.Close() }()

	identity, err := a.hello(ctx, sess)
	if err != nil {
		return false, err
	}

	a.setSession(sess, identity)
	defer a.clearSession(sess)

	a.log.Info("link: linked to hub", "peer", identity, "remote", sess.RemoteAddr())
	if a.cfg.OnLink != nil {
		a.cfg.OnLink(sess)
	}
	if a.cfg.OnUnlink != nil {
		defer a.cfg.OnUnlink()
	}

	select {
	case <-sess.Done():
	case <-ctx.Done():
	}
	return true, nil
}

// hello performs the handshake and returns the identity the hub bound the
// session to.
func (a *Agent) hello(ctx context.Context, sess *Session) (PeerID, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.HelloTimeout)
	defer cancel()

	token, err := a.cfg.Token(ctx)
	if err != nil {
		return PeerID{}, err
	}

	resp, err := linkv1.NewLinkServiceClient(sess.Conn()).Hello(withBearerToken(ctx, token), &linkv1.HelloRequest{
		Kind:         string(a.cfg.ID.Kind),
		Name:         a.cfg.ID.Name,
		InstanceUid:  a.cfg.InstanceUID,
		Capabilities: a.cfg.Capabilities,
		Version:      a.cfg.Version,
	})
	if err != nil {
		// FromStatus turns the hub's status back into the atlas sentinel it
		// stands for, so a caller inspecting this error matches the same
		// conditions it would in process.
		return PeerID{}, fmt.Errorf("link: hello: %w", class.FromStatus(err))
	}

	identity := PeerID{Kind: PeerKind(resp.GetKind()), Name: resp.GetName()}
	if identity != a.cfg.ID {
		// Not fatal — the hub's answer is the one that counts — but it means
		// this peer's own view of itself is wrong, which is worth knowing
		// before someone debugs why calls go somewhere unexpected.
		a.log.Warn("link: hub identified this peer differently than it claimed",
			"claimed", a.cfg.ID, "verified", identity)
	}
	return identity, nil
}

func (a *Agent) setSession(s *Session, identity PeerID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session, a.identity = s, identity
}

func (a *Agent) clearSession(s *Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == s {
		a.session = nil
	}
}

// jitter spreads a backoff over the top half of its interval: long enough to be
// a real wait, random enough that peers dropped together do not return together.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}