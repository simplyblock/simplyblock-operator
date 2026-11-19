// The swap protocol over the wire: what the operator sends reaches the node's
// Swapper intact, the node's answers come back, and its refusals keep their
// meaning so the operator can tell a volume that cannot swap from a namespace
// that did not appear.

package swaprpc

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/volstack/layers"
)

func (r *rig) serve() *Client {
	r.t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	NewServer(r.swapper).Register(srv)
	go func() { _ = srv.Serve(lis) }()
	r.t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(c context.Context, _ string) (net.Conn, error) { return lis.DialContext(c) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	return Remote(conn)
}

// U-38 (service half): the node advertises the capability the operator checks.
func TestTheServiceAdvertisesTheVolumeSwapCapability(t *testing.T) {
	if !slices.Contains(Capabilities(), "volume-swap") {
		t.Fatalf("Capabilities() = %v, want volume-swap", Capabilities())
	}
}

func TestAFullSwapRoundTripsOverTheWire(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", true)
	c := r.serve()

	if _, err := c.Prepare(ctx, []string{handle(volA)}, targetNQN, targetPaths()); err != nil {
		t.Fatal(err)
	}
	if got := r.connector.connected[0]; len(got) != 2 || got[0].Address != "10.0.0.1" || got[0].NQN != targetNQN {
		t.Fatalf("the node connected %+v", got)
	}
	states, err := c.Suspend(ctx, []string{handle(volA)}, r.clock.Now().Add(time.Hour))
	if err != nil || !states[0].Suspended {
		t.Fatalf("Suspend = %+v, %v", states, err)
	}
	r.moveNamespace(volA, "259:7", 3)
	states, err = c.Resume(ctx, []string{handle(volA)}, targetNQN)
	if err != nil || states[0].Suspended || states[0].NQN != targetNQN {
		t.Fatalf("Resume = %+v, %v", states, err)
	}
	if r.mapper.deviceOf(layers.DMLinearName(volA)) != "259:7" {
		t.Fatal("the mapping was not re-pointed")
	}
	done, err := c.Release(ctx, sourceNQN)
	if err != nil || !done {
		t.Fatalf("Release = %t, %v", done, err)
	}
	described, err := c.Describe(ctx, []string{handle(volA)})
	if err != nil || len(described) != 1 || described[0].NamespaceUUID != volA || !described[0].Swappable {
		t.Fatalf("Describe = %+v, %v", described, err)
	}
}

func TestRefusalsKeepTheirMeaningAcrossTheWire(t *testing.T) {
	r := newRig(t)
	r.stage(volA, "259:1", false)
	r.stage(volB, "259:2", true)
	c := r.serve()

	if _, err := c.Suspend(ctx, []string{handle(volA)}, r.clock.Now().Add(time.Hour)); !errors.Is(err, ErrNotSwappable) {
		t.Errorf("not swappable: err = %v", err)
	}
	if _, err := c.Suspend(ctx, []string{handle(volB)}, r.clock.Now().Add(-time.Second)); !errors.Is(err, ErrDeadlinePassed) {
		t.Errorf("past deadline: err = %v", err)
	}
	if _, err := c.Suspend(ctx, []string{handle(volB)}, r.clock.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	r.devices.remove(sourceNQN)
	if _, err := c.Resume(ctx, []string{handle(volB)}, targetNQN); !errors.Is(err, ErrNamespaceNotFound) {
		t.Errorf("namespace missing: err = %v", err)
	}
}
