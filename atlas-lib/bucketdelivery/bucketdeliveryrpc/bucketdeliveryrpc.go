// Package bucketdeliveryrpc carries an S3 bucket's mount delivery over a link:
// the node serves it, and the operator calls it.
//
// The mutating counterpart to storagerpc, and a separate service rather than
// more methods on it, for the same reason nfsexportrpc is: storagerpc is
// read-only by construction, and keeping them apart lets a credential be
// granted the reading and not the writing.
//
// On the node, the CSI node plugin's own NodeServer satisfies Deliverer by
// calling its existing NodeStageVolume/NodePublishVolume (Deliver) and
// NodeUnpublishVolume/NodeUnstageVolume (Withdraw) in-process: no new mount
// capability, no new component, and no new record. The same stack record
// that already heals a kubelet-staged volume after a restart covers a
// bucket this way too (design-bucket-provisioning.md §5.2):
//
//	srv, err := bucketdeliveryrpc.NewServer(nodeServer) // nodeServer satisfies Deliverer
//	agent, err := link.NewAgent(link.AgentConfig{
//	    Register:     srv.Register,
//	    Capabilities: bucketdeliveryrpc.Capabilities(),
//	    // ...
//	})
//
// The direction reads backward: the node dials the operator and the operator
// calls back down it. So a node with no live session is normal rather than an
// error, and callers get link.ErrNoSession and requeue.
package bucketdeliveryrpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/bucketdelivery/bucketdeliveryrpc/bucketdeliveryv1"
	"github.com/simplyblock/atlas/errs/class"
)

// CapabilityBucketDelivery is what a node advertises when it can deliver and
// withdraw a bucket's mount.
const CapabilityBucketDelivery = "atlas.bucketdelivery.v1.BucketDeliveryService"

// Capabilities names what this package serves, for the link handshake.
func Capabilities() []string { return []string{CapabilityBucketDelivery} }

// Bucket is one bucket's mount delivery: which volume, and the context the
// node needs to stage and publish it. It mirrors nfsexport.Spec's role: the
// plain value both the RPC and its caller agree on.
type Bucket struct {
	// Name is the S3 bucket name the mount path is derived from.
	Name string
	// VolumeHandle is the CSI volume handle of the bucket's backing volume.
	VolumeHandle string
	// VolumeContext mirrors CSI's own VolumeContext field for field, so a
	// Deliverer can pass it straight into NodeStageVolume/NodePublishVolume.
	VolumeContext map[string]string
}

// Deliverer is the node-side work. A CSI node plugin's NodeServer satisfies
// it by wrapping its own stage/publish and unpublish/unstage calls.
type Deliverer interface {
	Deliver(ctx context.Context, bucket Bucket) error
	Withdraw(ctx context.Context, bucket Bucket) error
}

// Server serves BucketDeliveryService over a link.
type Server struct {
	bucketdeliveryv1.UnimplementedBucketDeliveryServiceServer
	deliverer Deliverer
}

// NewServer refuses a nil deliverer rather than failing on the first call: a
// node that advertises the capability and cannot honor it is worse than one
// that never advertised it.
func NewServer(deliverer Deliverer) (*Server, error) {
	if deliverer == nil {
		return nil, fmt.Errorf("bucketdeliveryrpc: no deliverer")
	}
	return &Server{deliverer: deliverer}, nil
}

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	bucketdeliveryv1.RegisterBucketDeliveryServiceServer(r, s)
}

// DeliverBucket stages and publishes the bucket's volume on this node.
func (s *Server) DeliverBucket(
	ctx context.Context, req *bucketdeliveryv1.DeliverBucketRequest,
) (*bucketdeliveryv1.DeliverBucketResponse, error) {
	if err := s.deliverer.Deliver(ctx, bucketFromDeliverProto(req)); err != nil {
		return nil, class.Status(err)
	}
	return &bucketdeliveryv1.DeliverBucketResponse{}, nil
}

// WithdrawBucket unpublishes and unstages the bucket's volume on this node.
func (s *Server) WithdrawBucket(
	ctx context.Context, req *bucketdeliveryv1.WithdrawBucketRequest,
) (*bucketdeliveryv1.WithdrawBucketResponse, error) {
	bucket := Bucket{Name: req.GetBucketName(), VolumeHandle: req.GetVolumeHandle()}
	if err := s.deliverer.Withdraw(ctx, bucket); err != nil {
		return nil, class.Status(err)
	}
	return &bucketdeliveryv1.WithdrawBucketResponse{}, nil
}

// Client reaches one node's BucketDeliveryService.
type Client struct {
	client bucketdeliveryv1.BucketDeliveryServiceClient
}

// Remote returns a Client over an established link connection.
func Remote(conn grpc.ClientConnInterface) *Client {
	return &Client{client: bucketdeliveryv1.NewBucketDeliveryServiceClient(conn)}
}

// Deliver asks the far node to stage and publish the bucket's volume.
func (c *Client) Deliver(ctx context.Context, bucket Bucket) error {
	_, err := c.client.DeliverBucket(ctx, &bucketdeliveryv1.DeliverBucketRequest{
		BucketName:    bucket.Name,
		VolumeHandle:  bucket.VolumeHandle,
		VolumeContext: bucket.VolumeContext,
	})
	if err != nil {
		return fmt.Errorf("node: deliver bucket %s: %w", bucket.Name, class.FromStatus(err))
	}
	return nil
}

// Withdraw asks the far node to unpublish and unstage the bucket's volume.
func (c *Client) Withdraw(ctx context.Context, bucket Bucket) error {
	_, err := c.client.WithdrawBucket(ctx, &bucketdeliveryv1.WithdrawBucketRequest{
		BucketName:   bucket.Name,
		VolumeHandle: bucket.VolumeHandle,
	})
	if err != nil {
		return fmt.Errorf("node: withdraw bucket %s: %w", bucket.Name, class.FromStatus(err))
	}
	return nil
}

func bucketFromDeliverProto(req *bucketdeliveryv1.DeliverBucketRequest) Bucket {
	return Bucket{
		Name:          req.GetBucketName(),
		VolumeHandle:  req.GetVolumeHandle(),
		VolumeContext: req.GetVolumeContext(),
	}
}
