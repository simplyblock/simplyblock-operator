package link

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/link/linkv1"
)

const testToken = "shared-test-token"

// Health is used as the stand-in service in both directions: it is a real gRPC
// service that ships with the library, so a call over the link exercises the
// genuine path — codec, deadlines, status codes — without this package having
// to define a protocol for its own tests.
func registerHealth(srv grpc.ServiceRegistrar) {
	h := health.NewServer()
	h.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, h)
}

func checkHealth(t *testing.T, conn grpc.ClientConnInterface) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check over link: %v", err)
	}
	return resp.GetStatus()
}

// quietLogger keeps the link's lifecycle logging out of test output. Sessions
// outlive individual tests during cleanup, so logging to t would risk writing
// to a finished test.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startHub(t *testing.T, mutate func(*HubConfig)) *Hub {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	cfg := HubConfig{
		Listener:         lis,
		Auth:             InsecureStaticAuthenticator{Token: testToken},
		HandshakeTimeout: 2 * time.Second,
		Capabilities:     []string{"hub.test"},
		Version:          "hub-test",
		Logger:           quietLogger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}

	hub, err := NewHub(cfg)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = hub.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	return hub
}

func startAgent(t *testing.T, hub *Hub, mutate func(*AgentConfig)) *Agent {
	t.Helper()

	cfg := AgentConfig{
		Dial:         InsecureDialer(hub.Addr().String()),
		ID:           NodePeer("worker-1"),
		InstanceUID:  "instance-1",
		Token:        StaticToken(testToken),
		Register:     registerHealth,
		Capabilities: []string{"node.test"},
		Version:      "agent-test",
		HelloTimeout: 5 * time.Second,
		MinBackoff:   10 * time.Millisecond,
		MaxBackoff:   50 * time.Millisecond,
		Logger:       quietLogger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}

	agent, err := NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = agent.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-ran
	})
	return agent
}

// waitFor polls cond until it holds, failing the test if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// neverWithin fails if cond becomes true inside d. It is how the tests assert a
// rejection: nothing happening is the expected outcome, and only time
// distinguishes that from something that has not happened yet.
func neverWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("expected %s never to happen, but it did", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForPeer(t *testing.T, hub *Hub, id PeerID) *Peer {
	t.Helper()
	var peer *Peer
	waitFor(t, "peer "+id.String()+" to link", func() bool {
		p, err := hub.Registry().Peer(id)
		if err != nil {
			return false
		}
		peer = p
		return true
	})
	return peer
}

// TestHubCallsPeerOverReverseLink is the property the whole package exists for:
// the peer opened the connection, and the hub is the one issuing RPCs on it.
func TestHubCallsPeerOverReverseLink(t *testing.T) {
	hub := startHub(t, nil)
	startAgent(t, hub, nil)

	peer := waitForPeer(t, hub, NodePeer("worker-1"))
	if got := checkHealth(t, peer.Conn()); got != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health over reverse link = %v, want SERVING", got)
	}

	if !peer.HasCapability("node.test") {
		t.Errorf("peer capabilities = %v, want to contain node.test", peer.Capabilities)
	}
	if peer.Version != "agent-test" {
		t.Errorf("peer version = %q, want agent-test", peer.Version)
	}
}

// TestPeerCallsHubOverSameSession covers the other direction on the same
// connection — what makes hub-side services a later registration rather than a
// second transport.
func TestPeerCallsHubOverSameSession(t *testing.T) {
	hub := startHub(t, func(cfg *HubConfig) { cfg.Register = registerHealth })
	agent := startAgent(t, hub, nil)

	waitForPeer(t, hub, NodePeer("worker-1"))
	waitFor(t, "agent to report itself linked", agent.Linked)

	conn, err := agent.Conn()
	if err != nil {
		t.Fatalf("agent conn: %v", err)
	}
	if got := checkHealth(t, conn); got != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health over forward link = %v, want SERVING", got)
	}
}

// TestIdentityComesFromAuthenticatorNotClaim is the security property: a peer
// registers as whoever its credentials say it is, not whoever it says it is.
func TestIdentityComesFromAuthenticatorNotClaim(t *testing.T) {
	hub := startHub(t, func(cfg *HubConfig) {
		cfg.Auth = authenticatorFunc(func(context.Context, string, Claim) (Identity, error) {
			return Identity{ID: NodePeer("worker-9"), InstanceUID: "verified-uid"}, nil
		})
	})
	agent := startAgent(t, hub, func(cfg *AgentConfig) { cfg.ID = NodePeer("worker-1") })

	peer := waitForPeer(t, hub, NodePeer("worker-9"))
	if peer.InstanceUID != "verified-uid" {
		t.Errorf("peer instance = %q, want verified-uid", peer.InstanceUID)
	}
	if _, err := hub.Registry().Peer(NodePeer("worker-1")); !errors.Is(err, ErrNoSession) {
		t.Errorf("claimed identity was registered: %v", err)
	}

	waitFor(t, "agent to adopt the verified identity", func() bool {
		return agent.Identity() == NodePeer("worker-9")
	})
}

// TestHelloRejectedWhenNotAccepting covers the leader gate: a replica that is
// not leading turns peers away so they redial to the one that is.
func TestHelloRejectedWhenNotAccepting(t *testing.T) {
	hub := startHub(t, func(cfg *HubConfig) { cfg.Accepting = func() bool { return false } })
	agent := startAgent(t, hub, nil)

	neverWithin(t, time.Second, "a peer to be registered", func() bool {
		return hub.Registry().Len() > 0
	})
	if agent.Linked() {
		t.Error("agent reports itself linked to a hub that refused it")
	}
}

// TestBadTokenIsRejected covers credentials that do not check out.
func TestBadTokenIsRejected(t *testing.T) {
	hub := startHub(t, nil)
	startAgent(t, hub, func(cfg *AgentConfig) { cfg.Token = StaticToken("wrong-token") })

	neverWithin(t, time.Second, "a peer to be registered", func() bool {
		return hub.Registry().Len() > 0
	})
}

// TestUnidentifiedSessionIsClosed covers a peer that connects and then says
// nothing: the hub must not accumulate anonymous connections.
func TestUnidentifiedSessionIsClosed(t *testing.T) {
	hub := startHub(t, func(cfg *HubConfig) { cfg.HandshakeTimeout = 250 * time.Millisecond })

	sess := dialSession(t, hub)
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("hub kept an unidentified session open past its handshake timeout")
	}
}

// TestSupersedesEarlierInstance covers a restarted peer whose predecessor left
// a session behind: the new one wins and the old one is torn down.
func TestSupersedesEarlierInstance(t *testing.T) {
	hub := startHub(t, nil)
	// A long backoff keeps the first agent from racing its own replacement:
	// the point here is which session the registry keeps, not how fast a peer
	// comes back.
	agent := startAgent(t, hub, func(cfg *AgentConfig) {
		cfg.MinBackoff = time.Hour
		cfg.MaxBackoff = time.Hour
	})

	first := waitForPeer(t, hub, NodePeer("worker-1"))
	if first.InstanceUID != "instance-1" {
		t.Fatalf("first peer instance = %q, want instance-1", first.InstanceUID)
	}

	sess := dialSession(t, hub)
	if _, err := sayHello(t, sess, NodePeer("worker-1"), "instance-2", testToken); err != nil {
		t.Fatalf("second hello: %v", err)
	}

	waitFor(t, "the newer instance to supersede", func() bool {
		p, err := hub.Registry().Peer(NodePeer("worker-1"))
		return err == nil && p.InstanceUID == "instance-2"
	})
	if n := hub.Registry().Len(); n != 1 {
		t.Errorf("registry holds %d peers, want 1", n)
	}
	waitFor(t, "the superseded session to be closed", func() bool { return !agent.Linked() })
}

// TestSessionLossDeregistersPeer covers the hub noticing a peer go away.
func TestSessionLossDeregistersPeer(t *testing.T) {
	hub := startHub(t, nil)
	startAgent(t, hub, func(cfg *AgentConfig) {
		cfg.MinBackoff = time.Hour
		cfg.MaxBackoff = time.Hour
	})

	peer := waitForPeer(t, hub, NodePeer("worker-1"))
	if err := peer.Close(); err != nil {
		t.Fatalf("closing peer session: %v", err)
	}

	waitFor(t, "the peer to be deregistered", func() bool { return hub.Registry().Len() == 0 })
}

// TestAgentReconnectsAfterSessionLoss covers the reconnect loop: losing the hub
// is a reason to redial, not to stop.
func TestAgentReconnectsAfterSessionLoss(t *testing.T) {
	hub := startHub(t, nil)
	startAgent(t, hub, nil)

	first := waitForPeer(t, hub, NodePeer("worker-1"))
	if err := first.Close(); err != nil {
		t.Fatalf("closing peer session: %v", err)
	}

	waitFor(t, "the agent to link again", func() bool {
		p, err := hub.Registry().Peer(NodePeer("worker-1"))
		return err == nil && p != first
	})

	second, err := hub.Registry().Peer(NodePeer("worker-1"))
	if err != nil {
		t.Fatalf("peer after reconnect: %v", err)
	}
	if got := checkHealth(t, second.Conn()); got != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health over reconnected link = %v, want SERVING", got)
	}
}

// TestAbsentPeerIsRetryable pins the classification a reconciler depends on: a
// peer that is not linked is a wait, not a failure.
func TestAbsentPeerIsRetryable(t *testing.T) {
	hub := startHub(t, nil)

	_, err := hub.Registry().Conn(NodePeer("never-connected"))
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}

	c := class.Of(err)
	if c.Code != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", c.Code)
	}
	if !c.Retryable {
		t.Error("an absent peer classified as permanent; reconcilers would fail instead of requeueing")
	}
}

// TestSecondHelloOnOneSessionIsRefused covers a peer trying to re-identify a
// session that is already bound to someone.
func TestSecondHelloOnOneSessionIsRefused(t *testing.T) {
	hub := startHub(t, nil)

	sess := dialSession(t, hub)
	if _, err := sayHello(t, sess, NodePeer("worker-1"), "instance-1", testToken); err != nil {
		t.Fatalf("first hello: %v", err)
	}

	_, err := sayHello(t, sess, NodePeer("worker-2"), "instance-2", testToken)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second hello error = %v, want FailedPrecondition", err)
	}
	if _, err := hub.Registry().Peer(NodePeer("worker-2")); !errors.Is(err, ErrNoSession) {
		t.Error("a second hello registered a second identity for one session")
	}
}

// TestHubShutdownClosesSessions covers a hub going away — losing leadership,
// or shutting down — dropping its peers so they redial elsewhere.
func TestHubShutdownClosesSessions(t *testing.T) {
	hub := startHub(t, nil)
	startAgent(t, hub, func(cfg *AgentConfig) {
		cfg.MinBackoff = time.Hour
		cfg.MaxBackoff = time.Hour
	})

	peer := waitForPeer(t, hub, NodePeer("worker-1"))
	if err := hub.Close(); err != nil {
		t.Fatalf("closing hub: %v", err)
	}

	select {
	case <-peer.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("hub shutdown left a peer session open")
	}
}

// dialSession opens a link to the hub by hand, without an Agent, for the tests
// that need to drive the handshake themselves.
func dialSession(t *testing.T, hub *Hub) *Session {
	t.Helper()

	raw, err := net.Dial("tcp", hub.Addr().String())
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	sess, err := newSession(raw, true, sessionConfig{name: "test"})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func sayHello(t *testing.T, sess *Session, id PeerID, uid, token string) (*linkv1.HelloResponse, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return linkv1.NewLinkServiceClient(sess.Conn()).Hello(withBearerToken(ctx, token), &linkv1.HelloRequest{
		Kind:        string(id.Kind),
		Name:        id.Name,
		InstanceUid: uid,
	})
}

// authenticatorFunc adapts a function to [Authenticator].
type authenticatorFunc func(ctx context.Context, token string, claim Claim) (Identity, error)

func (f authenticatorFunc) Authenticate(ctx context.Context, token string, claim Claim) (Identity, error) {
	return f(ctx, token, claim)
}