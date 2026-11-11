// The role service's wire layer: that a promotion survives the round trip with
// every field the agent acts on, and that the outcomes the operator tells
// apart arrive as the outcomes they were sent as. What the agent does with a
// promotion is tested where the agent is.
package nfsexportrpc

import (
	"context"
	"net"
	"reflect"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nfsexport"
)

type recordingMDS struct {
	described Description
	result    PromoteResult
	promoted  []PromoteRequest
}

func (m *recordingMDS) Describe(context.Context) (Description, error) { return m.described, nil }
func (m *recordingMDS) Promote(_ context.Context, r PromoteRequest) (PromoteResult, error) {
	m.promoted = append(m.promoted, r)
	return m.result, nil
}

func serveMDS(t *testing.T, mds MDS) *MDSClient {
	t.Helper()
	srv, err := NewMDSServer(mds)
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
	return RemoteMDS(conn)
}

func TestDescribeCarriesRoleEpochAndKey(t *testing.T) {
	want := Description{Role: RoleActive, Epoch: 7, Key: 0x01ba781600000007}
	c := serveMDS(t, &recordingMDS{described: want})
	got, err := c.Describe(context.Background())
	if err != nil || got != want {
		t.Fatalf("Describe = %+v, %v; want %+v", got, err, want)
	}
}

func TestPromoteCarriesEveryFieldTheAgentActsOn(t *testing.T) {
	conn := &lvol.Connection{NQN: "nqn.x", NSID: 1, UUID: "u"}
	req := PromoteRequest{
		Epoch:          3,
		Key:            0x01ba781600000003,
		HostNQN:        "nqn.2014-08.io.simplyblock:uuid:abc",
		StatefulSetUID: "uid-1",
		StateVolume:    VolumeSpec{VolumeUUID: "sv", ClusterID: "c", PoolID: "p", Connection: conn},
		Exports: []nfsexport.Spec{
			{VolumeUUID: "e1", Path: "/exports/e1", FSID: "f1", Clients: []string{"10.0.0.0/24"}, ClusterID: "c", PoolID: "p", HostNQN: "nqn.h", Connection: conn},
		},
	}
	m := &recordingMDS{result: PromoteResult{Outcome: PromoteActive}}
	c := serveMDS(t, m)
	if _, err := c.Promote(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(m.promoted) != 1 || !reflect.DeepEqual(m.promoted[0], req) {
		t.Fatalf("reached the guest as %+v, want %+v", m.promoted, req)
	}
}

func TestPromoteOutcomesArriveAsSent(t *testing.T) {
	for _, want := range []PromoteResult{
		{Outcome: PromoteActive},
		{Outcome: PromoteCollision, Device: "/dev/nvme0n1", Step: "collision", Message: "host b holds the key"},
		{Outcome: PromoteSuperseded, Device: "/dev/nvme1n1", Step: "report"},
		{Outcome: PromoteFenceFailed, Device: "/dev/nvme2n1", Step: "preempt", Message: "boom"},
		{Outcome: PromoteStaleEpoch, Message: "epoch 2 is below 3"},
	} {
		c := serveMDS(t, &recordingMDS{result: want})
		got, err := c.Promote(context.Background(), PromoteRequest{Epoch: 1})
		if err != nil || got != want {
			t.Errorf("sent %+v, got %+v, %v", want, got, err)
		}
	}
}

func TestNewMDSServerRefusesNil(t *testing.T) {
	if _, err := NewMDSServer(nil); err == nil {
		t.Error("a nil MDS was accepted")
	}
}
