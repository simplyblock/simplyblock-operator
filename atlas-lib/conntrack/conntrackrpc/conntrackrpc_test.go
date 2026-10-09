// Tests for the flow service's wire layer: that a selector survives the round
// trip, that the count comes back, and that a selector the node would refuse is
// refused on both ends. What the host does with it is tested in the conntrack
// package.

package conntrackrpc

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/conntrack"
)

// recordingForgetter captures what reached the node.
type recordingForgetter struct {
	got   []conntrack.Selector
	count uint
	err   error
}

func (f *recordingForgetter) forget(sel conntrack.Selector) (uint, error) {
	f.got = append(f.got, sel)
	return f.count, f.err
}

func serve(t *testing.T, f *recordingForgetter) *Client {
	t.Helper()
	srv, err := NewServer(f.forget)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	srv.Register(grpcServer)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return Remote(conn)
}

var oldMDS = conntrack.Selector{
	Protocol: conntrack.TCP, DstPort: 2049, ReplySource: netip.MustParseAddr("10.244.3.215"),
}

func TestASelectorReachesTheNodeIntactAndTheCountComesBack(t *testing.T) {
	f := &recordingForgetter{count: 7}
	n, err := serve(t, f).Forget(context.Background(), oldMDS)
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if n != 7 {
		t.Errorf("deleted = %d, want 7", n)
	}
	if len(f.got) != 1 || f.got[0] != oldMDS {
		t.Errorf("node got %+v, want [%+v]", f.got, oldMDS)
	}
}

// A selector naming no address would ask the node for every flow on the port.
// The client refuses to send it, and the node refuses it if sent anyway.
func TestAnInvalidSelectorIsRefusedOnBothEnds(t *testing.T) {
	f := &recordingForgetter{}
	client := serve(t, f)
	if _, err := client.Forget(context.Background(), conntrack.Selector{Protocol: conntrack.TCP, DstPort: 2049}); !errors.Is(err, conntrack.ErrInvalidSelector) {
		t.Errorf("client Forget = %v, want ErrInvalidSelector", err)
	}
	srv, err := NewServer(f.forget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ForgetFlows(context.Background(), forgetRequest(conntrack.Selector{Protocol: 6, DstPort: 2049})); err == nil {
		t.Error("server accepted a selector with no address")
	}
	if len(f.got) != 0 {
		t.Errorf("node forgot %+v for an invalid selector", f.got)
	}
}

func TestANodeFailureComesBackAsAnError(t *testing.T) {
	f := &recordingForgetter{err: errors.New("operation not permitted")}
	if _, err := serve(t, f).Forget(context.Background(), oldMDS); err == nil {
		t.Error("Forget = nil, want the node's failure")
	}
}
