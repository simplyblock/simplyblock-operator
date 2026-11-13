package sitelink

// The leader's rolling restart asks the other site's operator to refresh a
// storage node's pod there, over the site link (design-stretched-sync-
// replication-ramen.md §11.3).

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/link/sitev1"
)

// siteServer answers RefreshNode with state and records what it was asked.
type siteServer struct {
	sitev1.UnimplementedSiteServiceServer
	state sitev1.RefreshNodeResponse_State
	asked chan *sitev1.RefreshNodeRequest
}

func (s *siteServer) RefreshNode(_ context.Context, req *sitev1.RefreshNodeRequest) (*sitev1.RefreshNodeResponse, error) {
	s.asked <- req
	return &sitev1.RefreshNodeResponse{State: s.state}, nil
}

func linkedPair(t *testing.T, peer *siteServer) *Link {
	t.Helper()
	registry := link.NewRegistry()
	relay := link.NewRelay(registry, quiet())
	hub := serveHub(t, link.HubConfig{Registry: registry, Register: relay.Register})
	a := startLink(t, forwarderFor(t, hub, "site-a"), "operator-a")
	b := &Link{
		Pod: "operator-b", Dial: link.InsecureDialer(forwarderFor(t, hub, "site-b")), Token: link.StaticToken(token),
		MinBackoff: 10 * time.Millisecond, Logger: quiet(),
		Register: func(srv grpc.ServiceRegistrar) { sitev1.RegisterSiteServiceServer(srv, peer) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = b.Start(ctx) }()
	t.Cleanup(cancel)
	eventually(t, "site-a's operator to reach site-b's", func() bool {
		return a.ProbePeer(context.Background(), "site-b") == nil
	})
	return a
}

// The request reaches the other site's operator with the cluster, the node,
// and the line in time, and its answer comes back as whether the pod is
// refreshed.
func TestARefreshIsAskedOfThePeerSitesOperator(t *testing.T) {
	since := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for state, want := range map[sitev1.RefreshNodeResponse_State]bool{
		sitev1.RefreshNodeResponse_STATE_IN_PROGRESS: false,
		sitev1.RefreshNodeResponse_STATE_REFRESHED:   true,
		// A node with no pod on the other site has nothing to refresh, as a
		// node of this site with no pod has not.
		sitev1.RefreshNodeResponse_STATE_NOT_FOUND: true,
	} {
		t.Run(state.String(), func(t *testing.T) {
			peer := &siteServer{state: state, asked: make(chan *sitev1.RefreshNodeRequest, 1)}
			a := linkedPair(t, peer)
			got, err := a.RefreshNode(context.Background(), "site-b", "cluster-1", "node-1", since)
			if err != nil || got != want {
				t.Fatalf("RefreshNode = %v, %v; want %v", got, err, want)
			}
			req := <-peer.asked
			if req.GetClusterUuid() != "cluster-1" || req.GetNode() != "node-1" || req.GetSinceUnix() != since.Unix() {
				t.Errorf("the peer was asked %+v", req)
			}
		})
	}
}

// A link that has not started cannot ask anyone.
func TestARefreshBeforeLinkingIsAnError(t *testing.T) {
	if _, err := (&Link{}).RefreshNode(context.Background(), "site-b", "c", "n", time.Now()); err == nil {
		t.Error("a refresh through a link that never started succeeded")
	}
}
