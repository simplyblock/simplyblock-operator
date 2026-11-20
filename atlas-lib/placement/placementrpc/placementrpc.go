// Package placementrpc carries member placement for a supercluster over a
// link: the operator serves it, and the CSI controller calls it at
// CreateVolume when a class names a supercluster rather than a cluster
// (design-superclusters.md §7.2).
//
// It runs from a peer to the hub, the reverse of the link's other services,
// because the answer is made of what the operator holds: the members, their
// capacity readings, their topology, and the placements made since the last
// reading. The hub admits the call only once the controller has said Hello,
// and the server refuses every peer that is not a CSI controller.
//
// On the operator:
//
//	srv, err := placementrpc.NewServer(placer)
//	hub, err := link.NewHub(link.HubConfig{Register: srv.Register, ...})
//
// On the CSI controller:
//
//	conn, err := agent.Conn()
//	member, err := placementrpc.Remote(conn).Place(ctx, req)
package placementrpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/link"
	"github.com/simplyblock/atlas/placement/placementrpc/placementv1"
)

// CapabilityPlacement is what the operator advertises when it places volumes.
const CapabilityPlacement = "atlas.placement.v1.PlacementService"

// Capabilities names what this package serves, for the link handshake.
func Capabilities() []string { return []string{CapabilityPlacement} }

// Request is one CreateVolume for a supercluster, as the CSI controller
// received it.
type Request struct {
	// AggregateID is the supercluster's identifier, from the class's
	// cluster_id.
	AggregateID string
	// PoolName is the pool, by name, which every member holds.
	PoolName string
	// VolumeName is the CSI volume name.
	VolumeName string
	// SizeBytes is the requested capacity.
	SizeBytes int64
	// Preferred and Requisite are the accessibility requirements' segments.
	Preferred []map[string]string
	Requisite []map[string]string
	// ClaimName and ClaimNamespace name the claim the placement is recorded on.
	ClaimName      string
	ClaimNamespace string
	// ConsistencyGroup is the claim's consistency-group label, or empty.
	ConsistencyGroup string
	// SourceClusterID is the backend cluster a clone or restore is made from,
	// or empty for a new volume.
	SourceClusterID string
}

// Placer chooses a member, answering its backend cluster UUID. An error that
// carries a gRPC status reaches the driver with that status.
type Placer interface {
	Place(ctx context.Context, req Request) (string, error)
}

// Server serves PlacementService over a link.
type Server struct {
	placementv1.UnimplementedPlacementServiceServer
	placer Placer
}

// NewServer refuses a nil placer rather than failing on the first call.
func NewServer(placer Placer) (*Server, error) {
	if placer == nil {
		return nil, fmt.Errorf("placementrpc: no placer")
	}
	return &Server{placer: placer}, nil
}

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	placementv1.RegisterPlacementServiceServer(r, s)
}

// Place answers the member for one volume, for a CSI controller only.
func (s *Server) Place(ctx context.Context, req *placementv1.PlaceRequest) (*placementv1.PlaceResponse, error) {
	caller, ok := link.PeerFromContext(ctx)
	if !ok || caller.Kind != link.PeerKindController {
		return nil, status.Errorf(codes.PermissionDenied,
			"placement: only a CSI controller places volumes, and %s is not one", caller)
	}
	member, err := s.placer.Place(ctx, requestFromProto(req))
	if err != nil {
		if _, isStatus := status.FromError(err); isStatus {
			return nil, err
		}
		return nil, class.Status(err)
	}
	return &placementv1.PlaceResponse{ClusterId: member}, nil
}

// Client reaches the operator's PlacementService.
type Client struct {
	client placementv1.PlacementServiceClient
}

// Remote returns a Client over an established link connection.
func Remote(conn grpc.ClientConnInterface) *Client {
	return &Client{client: placementv1.NewPlacementServiceClient(conn)}
}

// Place asks the operator for a volume's member. The error is the operator's
// status as it answered it, unwrapped, so the caller reads its code and
// message directly.
func (c *Client) Place(ctx context.Context, req Request) (string, error) {
	resp, err := c.client.Place(ctx, requestToProto(req))
	if err != nil {
		return "", err
	}
	return resp.GetClusterId(), nil
}

func requestToProto(req Request) *placementv1.PlaceRequest {
	return &placementv1.PlaceRequest{
		AggregateId:      req.AggregateID,
		PoolName:         req.PoolName,
		VolumeName:       req.VolumeName,
		SizeBytes:        req.SizeBytes,
		Preferred:        topologiesToProto(req.Preferred),
		Requisite:        topologiesToProto(req.Requisite),
		ClaimName:        req.ClaimName,
		ClaimNamespace:   req.ClaimNamespace,
		ConsistencyGroup: req.ConsistencyGroup,
		SourceClusterId:  req.SourceClusterID,
	}
}

func requestFromProto(req *placementv1.PlaceRequest) Request {
	return Request{
		AggregateID:      req.GetAggregateId(),
		PoolName:         req.GetPoolName(),
		VolumeName:       req.GetVolumeName(),
		SizeBytes:        req.GetSizeBytes(),
		Preferred:        topologiesFromProto(req.GetPreferred()),
		Requisite:        topologiesFromProto(req.GetRequisite()),
		ClaimName:        req.GetClaimName(),
		ClaimNamespace:   req.GetClaimNamespace(),
		ConsistencyGroup: req.GetConsistencyGroup(),
		SourceClusterID:  req.GetSourceClusterId(),
	}
}

func topologiesToProto(in []map[string]string) []*placementv1.Topology {
	if len(in) == 0 {
		return nil
	}
	out := make([]*placementv1.Topology, 0, len(in))
	for _, segments := range in {
		out = append(out, &placementv1.Topology{Segments: segments})
	}
	return out
}

func topologiesFromProto(in []*placementv1.Topology) []map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(in))
	for _, topology := range in {
		out = append(out, topology.GetSegments())
	}
	return out
}
