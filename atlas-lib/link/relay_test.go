package link

// The site link (design-stretched-sync-replication-ramen.md §7): a relay on the
// hub, a forwarder per site, and peers on both sites, all in one process. A peer
// on site a reaches a peer on site b through its own forwarder, the relay, and
// b's forwarder, and the call it makes is an ordinary gRPC call.

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/link/sitev1"
)

// site is one site's half of the fixture: its forwarder's local hub and its
// link to the relay.
type site struct {
	name     string
	local    *Hub
	upstream *Agent
}

// fabric is the relay and the sites linked to it.
type fabric struct {
	relayHub  *Hub
	accepting *atomic.Bool
	sites     map[string]*site
}

// startFabric starts a relay and a forwarder for each named site.
func startFabric(t *testing.T, names ...string) *fabric {
	t.Helper()
	f := &fabric{accepting: &atomic.Bool{}, sites: map[string]*site{}}
	f.accepting.Store(true)

	registry := NewRegistry()
	relay := NewRelay(registry, quietLogger())
	f.relayHub = startHub(t, func(cfg *HubConfig) {
		cfg.Registry = registry
		cfg.Accepting = f.accepting.Load
		cfg.Register = relay.Register
	})

	for _, name := range names {
		f.sites[name] = startSite(t, f.relayHub, name, nil)
	}
	return f
}

// startSite starts one site's forwarder. mutateUpstream adjusts its link to the
// relay, for the tests that break it.
func startSite(t *testing.T, relayHub *Hub, name string, mutateUpstream func(*AgentConfig)) *site {
	t.Helper()
	fwd := NewForwarder(ForwarderConfig{Site: name, Logger: quietLogger()})
	s := &site{name: name}
	s.local = startHub(t, func(cfg *HubConfig) { cfg.Register = fwd.RegisterLocal })
	fwd.SetLocal(s.local.Registry())
	s.upstream = startAgent(t, relayHub, func(cfg *AgentConfig) {
		cfg.ID = SitePeer(name)
		cfg.InstanceUID = "forwarder-" + name
		cfg.Register = fwd.RegisterUpstream
		if mutateUpstream != nil {
			mutateUpstream(cfg)
		}
	})
	fwd.SetUpstream(s.upstream)
	return s
}

// startOperator links an operator peer to its site's forwarder. It serves the
// health service to remote callers through an acceptor, and returns the link it
// calls out on.
func startOperator(t *testing.T, s *site, pod string) *Agent {
	t.Helper()
	acceptor := NewAcceptor()
	srv := grpc.NewServer()
	registerHealth(srv)
	go func() { _ = srv.Serve(acceptor) }()
	t.Cleanup(srv.Stop)

	return startAgent(t, s.local, func(cfg *AgentConfig) {
		cfg.ID = OperatorPeer(pod)
		cfg.InstanceUID = pod
		cfg.Register = acceptor.Register
	})
}

func waitLinked(t *testing.T, agents ...*Agent) {
	t.Helper()
	for _, a := range agents {
		waitFor(t, "link of "+a.cfg.ID.String(), a.Linked)
	}
}

// U-54: an operator on site a makes a unary call to the operator on site b,
// through its forwarder, the relay, and b's forwarder.
func TestARemoteCallCrossesTheRelay(t *testing.T) {
	f := startFabric(t, "a", "b")
	opA := startOperator(t, f.sites["a"], "operator-a")
	opB := startOperator(t, f.sites["b"], "operator-b")
	waitLinked(t, f.sites["a"].upstream, f.sites["b"].upstream, opA, opB)

	conn, err := RemoteConn(opA, Remote("b", PeerKindOperator, ""))
	if err != nil {
		t.Fatalf("RemoteConn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := checkHealth(t, conn); got != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health over the relay = %v, want SERVING", got)
	}
}

// U-55: a forwarder may speak for its own site's peers only. A source on
// another site, sent by site a's forwarder, is refused by the relay.
func TestTheRelayRefusesASourceOnAnotherSite(t *testing.T) {
	f := startFabric(t, "b")
	opB := startOperator(t, f.sites["b"], "operator-b")
	impostor := startAgent(t, f.relayHub, func(cfg *AgentConfig) {
		cfg.ID = SitePeer("a")
		cfg.InstanceUID = "impostor"
		cfg.Register = nil
	})
	waitLinked(t, f.sites["b"].upstream, opB, impostor)

	conn, err := impostor.Conn()
	if err != nil {
		t.Fatalf("relay conn: %v", err)
	}
	err = openRaw(t, conn, &sitev1.StreamEnds{Target: "site/b/operator", Source: "site/b/operator/forged"})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a source on site b from site a's forwarder: %v, want PermissionDenied", err)
	}
}

// U-56: a node plugin may not open a stream to another site; an operator may.
func TestANodePeerCannotOpenARemoteStream(t *testing.T) {
	f := startFabric(t, "a", "b")
	opB := startOperator(t, f.sites["b"], "operator-b")
	node := startAgent(t, f.sites["a"].local, func(cfg *AgentConfig) {
		cfg.ID = NodePeer("worker-1")
		cfg.Register = nil
	})
	waitLinked(t, f.sites["a"].upstream, f.sites["b"].upstream, opB, node)

	_, err := OpenRemote(context.Background(), node, Remote("b", PeerKindOperator, ""))
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a node peer opening a remote stream: %v, want PermissionDenied", err)
	}
}

// U-57: a target whose site is not linked is ErrNoSession's code, retryable,
// and nothing is left open.
func TestATargetOnAnUnlinkedSiteIsUnavailable(t *testing.T) {
	f := startFabric(t, "a")
	opA := startOperator(t, f.sites["a"], "operator-a")
	waitLinked(t, f.sites["a"].upstream, opA)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := OpenRemote(ctx, opA, Remote("b", PeerKindOperator, ""))
	if status.Code(err) != codes.Unavailable {
		t.Errorf("a stream to an unlinked site: %v, want Unavailable", err)
	}
}

// U-57, the second half: a linked site with no peer of the kind is refused the
// same way, by the target's forwarder.
func TestAnAbsentPeerOnALinkedSiteIsUnavailable(t *testing.T) {
	f := startFabric(t, "a", "b")
	opA := startOperator(t, f.sites["a"], "operator-a")
	waitLinked(t, f.sites["a"].upstream, f.sites["b"].upstream, opA)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := OpenRemote(ctx, opA, Remote("b", PeerKindOperator, ""))
	if status.Code(err) != codes.Unavailable {
		t.Errorf("a stream to a site with no operator linked: %v, want Unavailable", err)
	}
}

// U-58: the relay drops a site's session; the forwarder relinks and a stream
// opened after that succeeds.
func TestAForwarderRelinksAfterTheRelayDropsIt(t *testing.T) {
	f := startFabric(t, "a", "b")
	opA := startOperator(t, f.sites["a"], "operator-a")
	opB := startOperator(t, f.sites["b"], "operator-b")
	waitLinked(t, f.sites["a"].upstream, f.sites["b"].upstream, opA, opB)

	peer, err := f.relayHub.Registry().Peer(SitePeer("b"))
	if err != nil {
		t.Fatalf("site b on the relay: %v", err)
	}
	_ = peer.Close()
	waitFor(t, "site b to relink", func() bool {
		p, err := f.relayHub.Registry().Peer(SitePeer("b"))
		return err == nil && p != peer
	})

	conn, err := RemoteConn(opA, Remote("b", PeerKindOperator, ""))
	if err != nil {
		t.Fatalf("RemoteConn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := checkHealth(t, conn); got != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health after the relink = %v, want SERVING", got)
	}
}

// U-59: a forwarder whose token the relay rejects never links, and a local peer
// whose token its forwarder rejects never links either.
func TestBadCredentialsLinkNeitherSiteNorPeer(t *testing.T) {
	f := startFabric(t)
	bad := startSite(t, f.relayHub, "x", func(cfg *AgentConfig) { cfg.Token = StaticToken("wrong") })
	peer := startAgent(t, bad.local, func(cfg *AgentConfig) {
		cfg.ID = OperatorPeer("operator-x")
		cfg.Token = StaticToken("wrong")
		cfg.Register = nil
	})

	time.Sleep(300 * time.Millisecond)
	if bad.upstream.Linked() {
		t.Error("a forwarder with a rejected token linked to the relay")
	}
	if peer.Linked() {
		t.Error("a peer with a rejected token linked to its forwarder")
	}
	if _, err := f.relayHub.Registry().Peer(SitePeer("x")); err == nil {
		t.Error("the relay registered a site whose token it rejected")
	}
}

// U-60: a relay that is not accepting turns forwarders away; they redial and
// link once it accepts.
func TestAForwarderLinksOnceTheRelayAccepts(t *testing.T) {
	f := startFabric(t)
	f.accepting.Store(false)
	s := startSite(t, f.relayHub, "a", nil)

	time.Sleep(300 * time.Millisecond)
	if s.upstream.Linked() {
		t.Fatal("a forwarder linked to a relay that is not accepting")
	}
	f.accepting.Store(true)
	waitLinked(t, s.upstream)
}

// U-61: the relay going away while a stream is open ends the stream with an
// error rather than leaving the caller waiting.
func TestAnOpenStreamEndsWhenTheRelayGoesAway(t *testing.T) {
	f := startFabric(t, "a", "b")
	opA := startOperator(t, f.sites["a"], "operator-a")
	opB := startOperator(t, f.sites["b"], "operator-b")
	waitLinked(t, f.sites["a"].upstream, f.sites["b"].upstream, opA, opB)

	stream, err := OpenRemote(context.Background(), opA, Remote("b", PeerKindOperator, ""))
	if err != nil {
		t.Fatalf("OpenRemote: %v", err)
	}
	defer func() { _ = stream.Close() }()

	_ = f.relayHub.Close()
	read := make(chan error, 1)
	go func() {
		// The target's gRPC server writes its settings as soon as it accepts
		// the stream, so read past what arrived before the relay went away.
		buf := make([]byte, 4096)
		for {
			if _, err := stream.Read(buf); err != nil {
				read <- err
				return
			}
		}
	}()
	select {
	case err := <-read:
		if errors.Is(err, io.EOF) {
			t.Errorf("a stream across a closed relay ended as a clean EOF: %v; want an error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stream across a closed relay was left hanging")
	}
}

// openRaw opens a relay stream on conn with ends as its first frame and returns
// the status it was refused with, or nil once it is accepted.
func openRaw(t *testing.T, conn grpc.ClientConnInterface, ends *sitev1.StreamEnds) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := sitev1.NewRelayServiceClient(conn).Open(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&sitev1.OpenRequest{Kind: &sitev1.OpenRequest_Ends{Ends: ends}}); err != nil {
		return err
	}
	_, err = stream.Recv()
	return err
}

var _ net.Listener = (*Acceptor)(nil)
