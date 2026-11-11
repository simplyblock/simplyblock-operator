// Tests of the per-pod MDS client accessor: that it addresses the pod it is
// asked about, reports a missing session as one, and carries a promotion and
// a role query through to the guest.

package csilink

import (
	"context"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

type guestMDS struct {
	role     nfsexportrpc.Role
	promoted []nfsexportrpc.PromoteRequest
}

func (m *guestMDS) Describe(context.Context) (nfsexportrpc.Description, error) {
	return nfsexportrpc.Description{Role: m.role, Epoch: 3}, nil
}

func (m *guestMDS) Promote(_ context.Context, r nfsexportrpc.PromoteRequest) (nfsexportrpc.PromoteResult, error) {
	m.promoted = append(m.promoted, r)
	return nfsexportrpc.PromoteResult{Outcome: nfsexportrpc.PromoteActive}, nil
}

type fakeMDSRegistry map[link.PeerID]grpc.ClientConnInterface

func (r fakeMDSRegistry) Conn(id link.PeerID) (grpc.ClientConnInterface, error) {
	if c, ok := r[id]; ok {
		return c, nil
	}
	return nil, link.ErrNoSession
}

func serveGuest(t *testing.T, mds nfsexportrpc.MDS) grpc.ClientConnInterface {
	t.Helper()
	srv, err := nfsexportrpc.NewMDSServer(mds)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestEachPodIsReachedThroughItsOwnSession(t *testing.T) {
	zero, one := &guestMDS{role: nfsexportrpc.RoleActive}, &guestMDS{role: nfsexportrpc.RoleStandby}
	guests := NewMDSGuests(fakeMDSRegistry{
		link.MDSPeer("mds-0"): serveGuest(t, zero),
		link.MDSPeer("mds-1"): serveGuest(t, one),
	})

	for pod, want := range map[string]nfsexportrpc.Role{"mds-0": nfsexportrpc.RoleActive, "mds-1": nfsexportrpc.RoleStandby} {
		got, err := guests.Describe(context.Background(), pod)
		if err != nil || got.Role != want || got.Epoch != 3 {
			t.Errorf("Describe(%s) = %+v, %v, want role %v", pod, got, err, want)
		}
	}

	res, err := guests.Promote(context.Background(), "mds-1", nfsexportrpc.PromoteRequest{Epoch: 4})
	if err != nil || res.Outcome != nfsexportrpc.PromoteActive {
		t.Fatalf("Promote = %+v, %v", res, err)
	}
	if len(one.promoted) != 1 || one.promoted[0].Epoch != 4 || len(zero.promoted) != 0 {
		t.Errorf("promotions: mds-1=%v mds-0=%v, want the call at mds-1 alone", one.promoted, zero.promoted)
	}
}

func TestAPodWithNoSessionIsReportedAsSuch(t *testing.T) {
	guests := NewMDSGuests(fakeMDSRegistry{})
	if guests.HasSession("mds-0") {
		t.Error("HasSession = true for an unlinked pod")
	}
	_, err := guests.Describe(context.Background(), "mds-0")
	if !errors.Is(err, link.ErrNoSession) {
		t.Errorf("Describe error = %v, want link.ErrNoSession", err)
	}
	if _, err := guests.Promote(context.Background(), "mds-0", nfsexportrpc.PromoteRequest{}); !errors.Is(err, link.ErrNoSession) {
		t.Errorf("Promote error = %v, want link.ErrNoSession", err)
	}
}

func TestOnlyAnMDSPeerOfThatNameIsAGuest(t *testing.T) {
	// A node plugin that happens to share a name is not a metadata server.
	guests := NewMDSGuests(fakeMDSRegistry{link.NodePeer("mds-0"): serveGuest(t, &guestMDS{})})
	if guests.HasSession("mds-0") {
		t.Error("a node peer answered for an MDS pod")
	}
}
