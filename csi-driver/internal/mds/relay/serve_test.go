// Tests for what the runner offers the operator over the link: both services,
// each advertised, answering through the relay.

package relay

import (
	"context"
	"net"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc"
)

func TestTheRunnerServesExportsAndRolesAndAdvertisesBoth(t *testing.T) {
	want := nfsexportrpc.Description{Role: nfsexportrpc.RoleStandby, Epoch: 2, Key: 5}
	files := &guest{}
	m, tr := newMDS(&roleGuest{described: want}, files, standardResolver())
	register, capabilities, err := Services(tr, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{nfsexportrpc.CapabilityExports, nfsexportrpc.CapabilityMDS} {
		if !slices.Contains(capabilities, c) {
			t.Errorf("capabilities %v lack %s", capabilities, c)
		}
	}

	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	got, err := nfsexportrpc.RemoteMDS(conn).Describe(context.Background())
	if err != nil || got != want {
		t.Errorf("Describe = %+v, %v; want %+v", got, err, want)
	}
	if err := nfsexportrpc.Remote(conn).Create(context.Background(), spec()); err != nil {
		t.Errorf("CreateExport: %v", err)
	}
	if len(files.created) != 1 {
		t.Errorf("the export call reached the guest %d times, want once", len(files.created))
	}
}
