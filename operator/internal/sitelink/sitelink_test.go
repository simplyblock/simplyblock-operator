package sitelink

// The operator's end of the site link, over a relay and two forwarders in one
// process: each site's operator links to its forwarder, answers the other's
// probe, and reports a peer it cannot reach.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/simplyblock/atlas/link"
)

const token = "test-token"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serveHub(t *testing.T, cfg link.HubConfig) *link.Hub {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listener = lis
	cfg.Auth = link.InsecureStaticAuthenticator{Token: token}
	cfg.Logger = quiet()
	hub, err := link.NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = hub.Serve(ctx) }()
	t.Cleanup(cancel)
	return hub
}

func run(t *testing.T, a *link.Agent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = a.Run(ctx) }()
	t.Cleanup(cancel)
}

// forwarderFor starts a site's forwarder linked to the relay and returns the
// address its site's operator dials.
func forwarderFor(t *testing.T, relay *link.Hub, site string) string {
	t.Helper()
	fwd := link.NewForwarder(link.ForwarderConfig{Site: site, Logger: quiet()})
	local := serveHub(t, link.HubConfig{Register: fwd.RegisterLocal})
	fwd.SetLocal(local.Registry())
	up, err := link.NewAgent(link.AgentConfig{
		Dial: link.InsecureDialer(relay.Addr().String()), ID: link.SitePeer(site), Token: link.StaticToken(token),
		Register: fwd.RegisterUpstream, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fwd.SetUpstream(up)
	run(t, up)
	return local.Addr().String()
}

func startLink(t *testing.T, address, pod string) *Link {
	t.Helper()
	l := &Link{
		Pod: pod, Dial: link.InsecureDialer(address), Token: link.StaticToken(token),
		MinBackoff: 10 * time.Millisecond, Logger: quiet(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = l.Start(ctx) }()
	t.Cleanup(cancel)
	return l
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// One site's operator reaches the other's over the relay.
func TestAnOperatorReachesThePeerSitesOperator(t *testing.T) {
	registry := link.NewRegistry()
	relay := link.NewRelay(registry, quiet())
	hub := serveHub(t, link.HubConfig{Registry: registry, Register: relay.Register})
	a := startLink(t, forwarderFor(t, hub, "site-a"), "operator-a")
	startLink(t, forwarderFor(t, hub, "site-b"), "operator-b")

	eventually(t, "site-a's operator to reach site-b's", func() bool {
		return a.ProbePeer(context.Background(), "site-b") == nil
	})
}

// A peer site with no operator linked is an error, not a hang.
func TestAnUnlinkedPeerIsAnError(t *testing.T) {
	registry := link.NewRegistry()
	relay := link.NewRelay(registry, quiet())
	hub := serveHub(t, link.HubConfig{Registry: registry, Register: relay.Register})
	a := startLink(t, forwarderFor(t, hub, "site-a"), "operator-a")
	eventually(t, "site-a's operator to link", a.Linked)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.ProbePeer(ctx, "site-b"); err == nil {
		t.Error("a probe of a site with no linked operator succeeded")
	}
}

// A link that has not started reports the peer as unreachable.
func TestAProbeBeforeLinkingIsAnError(t *testing.T) {
	if err := (&Link{}).ProbePeer(context.Background(), "site-b"); err == nil {
		t.Error("a probe through a link that never started succeeded")
	}
}
