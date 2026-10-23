// Tests for the placement service's wire layer, run over a real link: a
// request survives the trip intact, only a CSI controller may ask, and the
// status codes the operator answers with reach the driver unchanged, because
// the driver decides from them what the provisioner is told.

package placementrpc

import (
	"context"
	"io"
	"log/slog"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/link"
)

const testToken = "placement-test-token"

// recordingPlacer answers with a fixed member or error and remembers the
// requests it was asked.
type recordingPlacer struct {
	mu       sync.Mutex
	requests []Request
	member   string
	err      error
}

func (p *recordingPlacer) Place(_ context.Context, req Request) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	return p.member, p.err
}

func (p *recordingPlacer) asked() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.requests...)
}

// linked starts a hub serving placer and an agent linked to it as id, and
// returns a client over the agent's link.
func linked(t *testing.T, placer Placer, id link.PeerID) *Client {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(placer)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := link.NewHub(link.HubConfig{
		Listener: lis,
		Auth:     link.InsecureStaticAuthenticator{Token: testToken},
		Register: srv.Register,
		Logger:   quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = hub.Serve(ctx) }()

	agent, err := link.NewAgent(link.AgentConfig{
		Dial:        link.InsecureDialer(lis.Addr().String()),
		ID:          id,
		InstanceUID: "instance-1",
		Token:       link.StaticToken(testToken),
		MinBackoff:  10 * time.Millisecond,
		MaxBackoff:  50 * time.Millisecond,
		Logger:      quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	go func() { defer close(ran); _ = agent.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-ran; <-served })

	deadline := time.Now().Add(10 * time.Second)
	for !agent.Linked() {
		if time.Now().After(deadline) {
			t.Fatal("the agent never linked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn, err := agent.Conn()
	if err != nil {
		t.Fatal(err)
	}
	return Remote(conn)
}

func aRequest() Request {
	return Request{
		AggregateID:      "0d6f1b2e-1f0a-4c71-9a0e-5b7d2c3e4f50",
		PoolName:         "tenant-a",
		VolumeName:       "pvc-1",
		SizeBytes:        10 << 30,
		Preferred:        []map[string]string{{"topology.kubernetes.io/region": "eu-west-1"}},
		Requisite:        []map[string]string{{"topology.kubernetes.io/region": "eu-west-1"}, {"topology.kubernetes.io/region": "eu-west-2"}},
		ClaimName:        "data",
		ClaimNamespace:   "app",
		ConsistencyGroup: "orders",
		SourceClusterID:  "9b210000-0000-4000-8000-00000000000b",
	}
}

// A controller's request reaches the operator whole, and the member it answers
// comes back.
func TestARequestSurvivesTheTrip(t *testing.T) {
	placer := &recordingPlacer{member: "4f0c0000-0000-4000-8000-00000000000a"}
	client := linked(t, placer, link.ControllerPeer("csi-controller-0"))

	member, err := client.Place(context.Background(), aRequest())

	if err != nil || member != placer.member {
		t.Fatalf("Place = %q, %v, want %q", member, err, placer.member)
	}
	asked := placer.asked()
	if len(asked) != 1 {
		t.Fatalf("the placer was asked %d times, want 1", len(asked))
	}
	if !reflect.DeepEqual(aRequest(), asked[0]) {
		t.Errorf("the operator received %+v, want %+v", asked[0], aRequest())
	}
}

// Only a CSI controller places volumes. A node plugin's call is refused before
// the placer is asked, because a placement records an answer on a claim.
func TestANodePeerIsRefused(t *testing.T) {
	placer := &recordingPlacer{member: "member"}
	client := linked(t, placer, link.NodePeer("worker-1"))

	_, err := client.Place(context.Background(), aRequest())

	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a node's Place = %v, want PermissionDenied", err)
	}
	if got := placer.asked(); len(got) != 0 {
		t.Errorf("the placer was asked %d times, want none", len(got))
	}
}

// The codes the operator answers with reach the driver unchanged, the message
// included, because the driver maps them to what the provisioner sees.
func TestTheOperatorsStatusReachesTheDriver(t *testing.T) {
	for _, code := range []codes.Code{
		codes.NotFound, codes.FailedPrecondition, codes.ResourceExhausted, codes.Unavailable,
	} {
		placer := &recordingPlacer{err: status.Error(code, "member prod-a is full")}
		client := linked(t, placer, link.ControllerPeer("csi-controller-0"))

		_, err := client.Place(context.Background(), aRequest())

		if status.Code(err) != code {
			t.Errorf("Place = %v, want %s", err, code)
		}
		if s, _ := status.FromError(err); s == nil || s.Message() != "member prod-a is full" {
			t.Errorf("the message did not survive: %v", err)
		}
	}
}
