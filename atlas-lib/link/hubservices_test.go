// The services a hub serves back to its peers: answered only for a session
// that has completed Hello, and told which peer is calling.

package link

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// callerHealth is a health service that remembers who called it.
type callerHealth struct {
	*health.Server

	mu     sync.Mutex
	caller PeerID
	known  bool
}

func (h *callerHealth) Check(
	ctx context.Context, req *healthpb.HealthCheckRequest,
) (*healthpb.HealthCheckResponse, error) {
	h.mu.Lock()
	h.caller, h.known = PeerFromContext(ctx)
	h.mu.Unlock()
	return h.Server.Check(ctx, req)
}

// A session that has not said Hello reaches none of the hub's services: the
// hub serves them from the moment the session exists, and a peer it has not
// identified is anyone who can reach the listener.
func TestHubServicesRefuseAnUnidentifiedSession(t *testing.T) {
	hub := startHub(t, func(cfg *HubConfig) { cfg.Register = registerHealth })
	sess := dialSession(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := healthpb.NewHealthClient(sess.Conn()).Check(ctx, &healthpb.HealthCheckRequest{})

	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("a call before Hello = %v, want Unauthenticated", err)
	}
}

// A hub service is told which peer called it, as the authenticator verified
// it, so a service open to one kind of peer can refuse the others.
func TestHubServicesSeeTheVerifiedCaller(t *testing.T) {
	served := &callerHealth{Server: health.NewServer()}
	hub := startHub(t, func(cfg *HubConfig) {
		cfg.Register = func(srv grpc.ServiceRegistrar) { healthpb.RegisterHealthServer(srv, served) }
		cfg.Auth = authenticatorFunc(func(context.Context, string, Claim) (Identity, error) {
			return Identity{ID: ControllerPeer("csi-controller-0"), InstanceUID: "uid"}, nil
		})
	})
	agent := startAgent(t, hub, nil)
	waitFor(t, "agent to report itself linked", agent.Linked)

	conn, err := agent.Conn()
	if err != nil {
		t.Fatalf("agent conn: %v", err)
	}
	checkHealth(t, conn)

	served.mu.Lock()
	defer served.mu.Unlock()
	if !served.known || served.caller != ControllerPeer("csi-controller-0") {
		t.Errorf("caller = %v (known %v), want the verified controller", served.caller, served.known)
	}
}
