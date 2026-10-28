// Tests for the bucket delivery service's wire layer: that a bucket survives
// the round trip intact, and that the two error shapes a caller has to tell
// apart arrive distinguishable on the far side.
//
// What a Deliverer does with a bucket (staging and publishing a real pNFS
// volume) is the CSI node plugin's own concern, proven on a live cluster
// (design-bucket-provisioning.md §11). What is pinned here is only that the
// wire does not quietly change what crossed it, because a dropped
// VolumeContext key would publish a bucket at the wrong export.
package bucketdeliveryrpc

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/simplyblock/atlas/errs"
)

// recordingDeliverer captures what reached the node.
type recordingDeliverer struct {
	delivered []Bucket
	withdrawn []Bucket
	err       error
}

func (d *recordingDeliverer) Deliver(_ context.Context, bucket Bucket) error {
	if d.err != nil {
		return d.err
	}
	d.delivered = append(d.delivered, bucket)
	return nil
}

func (d *recordingDeliverer) Withdraw(_ context.Context, bucket Bucket) error {
	if d.err != nil {
		return d.err
	}
	d.withdrawn = append(d.withdrawn, bucket)
	return nil
}

// serve starts the service on an in-memory listener and returns a client for it.
func serve(t *testing.T, deliverer Deliverer) *Client {
	t.Helper()
	srv, err := NewServer(deliverer)
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

var fullBucket = Bucket{
	Name:         "team-a-reports",
	VolumeHandle: "nfs:f0bb9077-78c4-4482-9ccf-a5693ce2df78:9d016dd4-34d7-42f0-b549-52a5af2f1399:3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
	VolumeContext: map[string]string{
		"access_protocol": "nfs",
		"export_service":  "nfsexp-8f1a2c0e3b.s3-system.svc.cluster.local",
		"export_path":     "/mnt/team-a-reports-3c81",
		"fsid":            "3c81a0f4-1d2b-4e77-9a01-5f6c8b2d0e13",
	},
}

// Regression-shaped: every VolumeContext key crosses. A bucket that loses
// export_service or export_path cannot be staged at all, and one that loses
// fsid publishes an export whose file handles do not survive a move.
func TestDeliverRoundTripsTheWholeBucket(t *testing.T) {
	deliverer := &recordingDeliverer{}
	client := serve(t, deliverer)

	if err := client.Deliver(context.Background(), fullBucket); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if len(deliverer.delivered) != 1 {
		t.Fatalf("deliverer saw %d delivers, want 1", len(deliverer.delivered))
	}
	if got := deliverer.delivered[0]; !reflect.DeepEqual(got, fullBucket) {
		t.Errorf("bucket did not survive the round trip:\n got %+v\nwant %+v", got, fullBucket)
	}
}

// Withdraw carries no VolumeContext: the node already knows what it staged,
// so a round trip to the client's own fields is all it needs.
func TestWithdrawRoundTripsNameAndHandle(t *testing.T) {
	deliverer := &recordingDeliverer{}
	client := serve(t, deliverer)

	if err := client.Withdraw(context.Background(), fullBucket); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	if len(deliverer.withdrawn) != 1 {
		t.Fatalf("deliverer saw %d withdraws, want 1", len(deliverer.withdrawn))
	}
	got := deliverer.withdrawn[0]
	if got.Name != fullBucket.Name || got.VolumeHandle != fullBucket.VolumeHandle {
		t.Errorf("withdrawn = %+v, want name %q and handle %q", got, fullBucket.Name, fullBucket.VolumeHandle)
	}
	if got.VolumeContext != nil {
		t.Errorf("withdrawn.VolumeContext = %v, want none: Withdraw carries no context", got.VolumeContext)
	}
}

// A device that is not there is not-found on the far side too, which is what
// lets the caller wait for it rather than give up on the delivery.
func TestMissingDeviceArrivesAsNotFound(t *testing.T) {
	client := serve(t, &recordingDeliverer{err: errs.ErrNotFound})

	err := client.Deliver(context.Background(), fullBucket)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want it to carry errs.ErrNotFound", err)
	}
}

// A failed withdraw is distinguishable from a failed deliver: the caller
// retries each against its own call, not the other's.
func TestWithdrawFailureArrivesAsAnError(t *testing.T) {
	client := serve(t, &recordingDeliverer{err: errors.New("bucketdelivery: unpublish: device or resource busy")})

	err := client.Withdraw(context.Background(), fullBucket)
	if err == nil {
		t.Fatal("Withdraw over a failing deliverer returned no error")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Errorf("error = %q, want it to carry the node's reason", err.Error())
	}
}

// A nil deliverer is refused at construction. A node that advertised the
// capability and cannot honor it is worse than one that never advertised it,
// because the operator would deliver a bucket to it.
func TestNewServerRefusesANilDeliverer(t *testing.T) {
	if _, err := NewServer(nil); err == nil {
		t.Fatal("NewServer(nil) succeeded")
	}
}

// The capability name is what the operator matches on to tell a node that can
// deliver buckets from one that is merely connected, so it is pinned rather
// than left to drift with a rename.
func TestCapabilityNameIsStable(t *testing.T) {
	caps := Capabilities()
	if len(caps) != 1 || caps[0] != "atlas.bucketdelivery.v1.BucketDeliveryService" {
		t.Errorf("capabilities = %v, want the BucketDeliveryService name", caps)
	}
}
