// fakePeerRegistry stands in for a real csi-link session (design-bucket-
// provisioning.md §5.2, §11): the real one is a node's CSI plugin dialing the
// operator over TLS and answering bucketdeliveryrpc on the connection it
// holds open, which needs a live cluster this harness does not have. This
// dials an in-process bucketdeliveryrpc server per node instead, over a
// bufconn listener, and records every DeliverBucket/WithdrawBucket call each
// one acknowledges.
//
// It satisfies the same narrow PeerRegistry shape nfsexport_assembler.go
// already defines in the legacy package (Conn(id) (grpc.ClientConnInterface,
// error)) rather than importing it: a second copy of a three-line interface in
// its own package is the ordinary Go shape for "the part of a dependency this
// package needs," not the duplication the extract-to-atlas-lib skill is about.

package objectstore

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/bucketdelivery/bucketdeliveryrpc"
	"github.com/simplyblock/atlas/link"
)

// PeerRegistry is the part of the link hub a bucket-delivery caller needs.
type PeerRegistry interface {
	Conn(id link.PeerID) (grpc.ClientConnInterface, error)
}

// fakePeerRegistry records every delivery and withdrawal its nodes
// acknowledge, keyed by node name.
type fakePeerRegistry struct {
	mu        sync.Mutex
	conns     map[link.PeerID]*grpc.ClientConn
	delivered map[string][]bucketdeliveryrpc.Bucket
	withdrawn map[string][]bucketdeliveryrpc.Bucket
}

// newFakePeerRegistry starts one in-process bucketdeliveryrpc server per
// named node, each over its own bufconn listener, and returns a registry
// whose Conn(link.NodePeer(name)) reaches it. Every server and connection is
// torn down when ctx is done.
func newFakePeerRegistry(ctx context.Context, t *testing.T, nodeNames ...string) *fakePeerRegistry {
	t.Helper()
	r := &fakePeerRegistry{
		conns:     make(map[link.PeerID]*grpc.ClientConn),
		delivered: make(map[string][]bucketdeliveryrpc.Bucket),
		withdrawn: make(map[string][]bucketdeliveryrpc.Bucket),
	}
	for _, name := range nodeNames {
		r.addNode(ctx, t, name)
	}
	return r
}

func (r *fakePeerRegistry) addNode(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	deliverer := &recordingDeliverer{registry: r, node: name}
	srv, err := bucketdeliveryrpc.NewServer(deliverer)
	if err != nil {
		t.Fatalf("bucketdeliveryrpc.NewServer: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	srv.Register(grpcServer)
	go func() { _ = grpcServer.Serve(lis) }()
	go func() {
		<-ctx.Done()
		grpcServer.Stop()
	}()

	conn, err := grpc.NewClient("passthrough:///"+name,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialing fake node %s: %v", name, err)
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	r.mu.Lock()
	r.conns[link.NodePeer(name)] = conn
	r.mu.Unlock()
}

// Conn satisfies PeerRegistry: link.ErrNoSession for a node this registry was
// never given, exactly like the real registry reports a node that never
// linked or whose session dropped.
func (r *fakePeerRegistry) Conn(id link.PeerID) (grpc.ClientConnInterface, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	conn, ok := r.conns[id]
	if !ok {
		return nil, link.ErrNoSession
	}
	return conn, nil
}

// Delivered returns the buckets node has had DeliverBucket called for, in
// call order.
func (r *fakePeerRegistry) Delivered(node string) []bucketdeliveryrpc.Bucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bucketdeliveryrpc.Bucket(nil), r.delivered[node]...)
}

// Withdrawn returns the buckets node has had WithdrawBucket called for, in
// call order.
func (r *fakePeerRegistry) Withdrawn(node string) []bucketdeliveryrpc.Bucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bucketdeliveryrpc.Bucket(nil), r.withdrawn[node]...)
}

// recordingDeliverer is one node's bucketdeliveryrpc.Deliverer: it
// acknowledges every call unconditionally and records it against the
// registry under its own node name.
type recordingDeliverer struct {
	registry *fakePeerRegistry
	node     string
}

func (d *recordingDeliverer) Deliver(_ context.Context, bucket bucketdeliveryrpc.Bucket) error {
	d.registry.mu.Lock()
	defer d.registry.mu.Unlock()
	d.registry.delivered[d.node] = append(d.registry.delivered[d.node], bucket)
	return nil
}

func (d *recordingDeliverer) Withdraw(_ context.Context, bucket bucketdeliveryrpc.Bucket) error {
	d.registry.mu.Lock()
	defer d.registry.mu.Unlock()
	d.registry.withdrawn[d.node] = append(d.registry.withdrawn[d.node], bucket)
	return nil
}

// W-08 acceptance: the fake PeerRegistry records and acknowledges a
// DeliverBucket/WithdrawBucket call per node without a real csi-link session.
func TestFakePeerRegistryRecordsDeliveryAndWithdrawalPerNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registry := newFakePeerRegistry(ctx, t, "node-a", "node-b")

	bucket := bucketdeliveryrpc.Bucket{
		Name:         "team-a-reports",
		VolumeHandle: fakeClusterID + ":" + fakePoolID + ":" + "cb2f293c-6d6f-4687-ad13-eb81fbec7314",
		VolumeContext: map[string]string{
			"access_protocol": "nfs",
			"export_service":  "nfsexp-8f1a2c0e3b.default.svc.cluster.local",
			"export_path":     "/var/lib/simplyblock/exports/team-a-reports",
		},
	}

	connA, err := registry.Conn(link.NodePeer("node-a"))
	if err != nil {
		t.Fatalf("Conn(node-a): %v", err)
	}
	if err := bucketdeliveryrpc.Remote(connA).Deliver(ctx, bucket); err != nil {
		t.Fatalf("Deliver to node-a: %v", err)
	}

	if _, err := registry.Conn(link.NodePeer("node-b")); err != nil {
		t.Fatalf("Conn(node-b): %v", err)
	}
	if got := registry.Delivered("node-a"); len(got) != 1 || got[0].Name != bucket.Name {
		t.Fatalf("node-a delivered = %+v, want exactly one delivery of %q", got, bucket.Name)
	}
	if got := registry.Delivered("node-b"); len(got) != 0 {
		t.Fatalf("node-b delivered = %+v, want none: delivery only went to node-a", got)
	}

	if err := bucketdeliveryrpc.Remote(connA).Withdraw(ctx, bucket); err != nil {
		t.Fatalf("Withdraw from node-a: %v", err)
	}
	if got := registry.Withdrawn("node-a"); len(got) != 1 || got[0].Name != bucket.Name {
		t.Fatalf("node-a withdrawn = %+v, want exactly one withdrawal of %q", got, bucket.Name)
	}

	if _, err := registry.Conn(link.NodePeer("node-c")); err == nil {
		t.Fatal("Conn for a node never given to the registry succeeded")
	}
}
