package freezerpc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeFreezer records what reached the node.
type fakeFreezer struct {
	handle, token string
	deadline      time.Duration
	frozen, held  bool
	err           error
}

func (f *fakeFreezer) Freeze(_ context.Context, handle, token string, d time.Duration) (bool, error) {
	f.handle, f.token, f.deadline = handle, token, d
	return f.frozen, f.err
}

func (f *fakeFreezer) Thaw(_ context.Context, handle, token string) (bool, error) {
	f.handle, f.token = handle, token
	return f.held, f.err
}

// serve runs register on an in-memory gRPC server and dials it.
func serve(t *testing.T, register func(grpc.ServiceRegistrar)) grpc.ClientConnInterface {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestTheNodeServerCarriesTheRequest(t *testing.T) {
	f := &fakeFreezer{frozen: true, held: true}
	srv, err := NewServer(f)
	if err != nil {
		t.Fatal(err)
	}
	client := Remote(serve(t, srv.Register))

	frozen, err := client.Freeze(context.Background(), "worker-1", "c:p:v:s3", "tok", 7*time.Second)
	if err != nil || !frozen {
		t.Fatalf("Freeze = %v, %v", frozen, err)
	}
	if f.handle != "c:p:v:s3" || f.token != "tok" || f.deadline != 7*time.Second {
		t.Errorf("the node received %+v", f)
	}
	held, err := client.Thaw(context.Background(), "worker-1", "c:p:v:s3", "tok")
	if err != nil || !held {
		t.Errorf("Thaw = %v, %v", held, err)
	}
}

// A status the node chose reaches the controller as that status, so the
// controller can tell a volume held by another snapshot from a node fault.
func TestTheNodesStatusSurvives(t *testing.T) {
	f := &fakeFreezer{err: status.Error(codes.Aborted, "held by another token")}
	srv, _ := NewServer(f)
	_, err := Remote(serve(t, srv.Register)).Freeze(context.Background(), "n", "h", "t", time.Second)
	if status.Code(err) != codes.Aborted {
		t.Errorf("err = %v, want Aborted", err)
	}
	f.err = errors.New("plain")
	_, err = Remote(serve(t, srv.Register)).Freeze(context.Background(), "n", "h", "t", time.Second)
	if status.Code(err) != codes.Internal {
		t.Errorf("err = %v, want Internal", err)
	}
}

// A hold that lasted and could not be resumed is reported as held: the caller's
// snapshots are consistent, and the node keeps the resume.
func TestAHeldThawWithAResumeErrorIsHeld(t *testing.T) {
	f := &fakeFreezer{held: true, err: errors.New("resume failed")}
	srv, _ := NewServer(f)
	held, err := Remote(serve(t, srv.Register)).Thaw(context.Background(), "n", "h", "t")
	if err != nil || !held {
		t.Errorf("Thaw = %v, %v; want held", held, err)
	}
}

// The relay suspends a workload on a node's behalf, so it answers a CSI
// controller and nobody else. A call that carries no link identity is refused
// before any node is reached.
func TestTheRelayRefusesACallerThatIsNotAController(t *testing.T) {
	reached := false
	relay := NewRelay(func(string) (grpc.ClientConnInterface, error) {
		reached = true
		return nil, errors.New("unused")
	})
	_, err := Remote(serve(t, relay.Register)).Freeze(context.Background(), "worker-1", "h", "t", time.Second)
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("err = %v, want PermissionDenied", err)
	}
	if reached {
		t.Error("the relay reached a node for an unidentified caller")
	}
}
