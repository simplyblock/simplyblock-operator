// The guest's role over the wire: Describe and Promote. The agent serves them,
// the runner relays them, and the operator calls them. The Go types here are
// what all three share, so a field that crosses the wire is added once.
package nfsexportrpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/simplyblock/atlas/errs/class"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nfsexport"
	"github.com/simplyblock/atlas/nfsexport/nfsexportrpc/nfsexportv1"
)

// CapabilityMDS is what a guest advertises when it can be promoted.
const CapabilityMDS = "atlas.nfsexport.v1.MDSService"

// Role is what a guest is doing.
type Role int

const (
	// RoleStandby is a guest that holds no connection, no mount, and no nfsd.
	RoleStandby Role = iota + 1
	// RolePromoting is a guest in the middle of a Promote.
	RolePromoting
	// RoleActive is the guest serving the exports.
	RoleActive
)

func (r Role) String() string {
	switch r {
	case RoleStandby:
		return "Standby"
	case RolePromoting:
		return "Promoting"
	case RoleActive:
		return "Active"
	}
	return "Unknown"
}

// Description is a guest's answer to Describe.
type Description struct {
	Role  Role
	Epoch uint64
	Key   uint64
}

// VolumeSpec is a volume the guest connects itself.
type VolumeSpec struct {
	VolumeUUID string
	ClusterID  string
	PoolID     string
	Connection *lvol.Connection
}

// PromoteRequest is what turns a standby into the active guest.
type PromoteRequest struct {
	Epoch          uint64
	Key            uint64
	HostNQN        string
	StatefulSetUID string
	StateVolume    VolumeSpec
	Exports        []nfsexport.Spec
}

// PromoteOutcome says why a promotion did or did not happen.
type PromoteOutcome int

const (
	// PromoteActive is a guest that is active at the requested epoch.
	PromoteActive PromoteOutcome = iota + 1
	// PromoteCollision is a key another host is already registered under.
	PromoteCollision
	// PromoteSuperseded is a key of the same StatefulSet with a higher epoch.
	PromoteSuperseded
	// PromoteFenceFailed is a step that failed on a device, with nothing mounted.
	PromoteFenceFailed
	// PromoteStaleEpoch is an epoch below the accepted one, or a different one
	// while the guest is active.
	PromoteStaleEpoch
)

func (o PromoteOutcome) String() string {
	switch o {
	case PromoteActive:
		return "Active"
	case PromoteCollision:
		return "Collision"
	case PromoteSuperseded:
		return "Superseded"
	case PromoteFenceFailed:
		return "FenceFailed"
	case PromoteStaleEpoch:
		return "StaleEpoch"
	}
	return "Unknown"
}

// PromoteResult is the guest's answer to Promote. Device and Step say where a
// fence failed.
type PromoteResult struct {
	Outcome PromoteOutcome
	Device  string
	Step    string
	Message string
}

// MDS is the guest-side work. The agent's role machine satisfies it.
type MDS interface {
	Describe(ctx context.Context) (Description, error)
	Promote(ctx context.Context, req PromoteRequest) (PromoteResult, error)
}

// MDSServer serves MDSService.
type MDSServer struct {
	nfsexportv1.UnimplementedMDSServiceServer
	mds MDS
}

// NewMDSServer returns a server for mds.
func NewMDSServer(mds MDS) (*MDSServer, error) {
	if mds == nil {
		return nil, fmt.Errorf("nfsexportrpc: MDS is nil")
	}
	return &MDSServer{mds: mds}, nil
}

// Register adds the service to a gRPC server.
func (s *MDSServer) Register(r grpc.ServiceRegistrar) {
	nfsexportv1.RegisterMDSServiceServer(r, s)
}

// Describe reports the guest's role.
func (s *MDSServer) Describe(ctx context.Context, _ *nfsexportv1.DescribeRequest) (*nfsexportv1.DescribeResponse, error) {
	d, err := s.mds.Describe(ctx)
	if err != nil {
		return nil, class.Status(err)
	}
	return &nfsexportv1.DescribeResponse{Role: roleToProto(d.Role), Epoch: d.Epoch, Key: d.Key}, nil
}

// Promote promotes the guest.
func (s *MDSServer) Promote(ctx context.Context, req *nfsexportv1.PromoteRequest) (*nfsexportv1.PromoteResponse, error) {
	res, err := s.mds.Promote(ctx, promoteFromProto(req))
	if err != nil {
		return nil, class.Status(err)
	}
	return &nfsexportv1.PromoteResponse{
		Outcome: outcomeToProto(res.Outcome),
		Device:  res.Device,
		Step:    res.Step,
		Message: res.Message,
	}, nil
}

// MDSClient reaches one guest's MDSService.
type MDSClient struct {
	client nfsexportv1.MDSServiceClient
}

// RemoteMDS returns a client over an established connection.
func RemoteMDS(conn grpc.ClientConnInterface) *MDSClient {
	return &MDSClient{client: nfsexportv1.NewMDSServiceClient(conn)}
}

// Describe asks the guest for its role.
func (c *MDSClient) Describe(ctx context.Context) (Description, error) {
	resp, err := c.client.Describe(ctx, &nfsexportv1.DescribeRequest{})
	if err != nil {
		return Description{}, fmt.Errorf("guest: describe: %w", class.FromStatus(err))
	}
	return Description{Role: roleFromProto(resp.GetRole()), Epoch: resp.GetEpoch(), Key: resp.GetKey()}, nil
}

// Promote asks the guest to become the active one.
func (c *MDSClient) Promote(ctx context.Context, req PromoteRequest) (PromoteResult, error) {
	resp, err := c.client.Promote(ctx, promoteToProto(req))
	if err != nil {
		return PromoteResult{}, fmt.Errorf("guest: promote at epoch %d: %w", req.Epoch, class.FromStatus(err))
	}
	return PromoteResult{
		Outcome: outcomeFromProto(resp.GetOutcome()),
		Device:  resp.GetDevice(),
		Step:    resp.GetStep(),
		Message: resp.GetMessage(),
	}, nil
}

func promoteToProto(r PromoteRequest) *nfsexportv1.PromoteRequest {
	out := &nfsexportv1.PromoteRequest{
		Epoch:          r.Epoch,
		Key:            r.Key,
		HostNqn:        r.HostNQN,
		StatefulsetUid: r.StatefulSetUID,
		StateVolume: &nfsexportv1.Volume{
			VolumeUuid: r.StateVolume.VolumeUUID,
			ClusterId:  r.StateVolume.ClusterID,
			PoolId:     r.StateVolume.PoolID,
			Connection: connectionToProto(r.StateVolume.Connection),
		},
	}
	for _, e := range r.Exports {
		out.Exports = append(out.Exports, specToProto(e))
	}
	return out
}

func promoteFromProto(r *nfsexportv1.PromoteRequest) PromoteRequest {
	sv := r.GetStateVolume()
	out := PromoteRequest{
		Epoch:          r.GetEpoch(),
		Key:            r.GetKey(),
		HostNQN:        r.GetHostNqn(),
		StatefulSetUID: r.GetStatefulsetUid(),
		StateVolume: VolumeSpec{
			VolumeUUID: sv.GetVolumeUuid(),
			ClusterID:  sv.GetClusterId(),
			PoolID:     sv.GetPoolId(),
			Connection: connectionFromProto(sv.GetConnection()),
		},
	}
	for _, e := range r.GetExports() {
		out.Exports = append(out.Exports, specFromProto(e))
	}
	return out
}

func roleToProto(r Role) nfsexportv1.Role {
	switch r {
	case RoleStandby:
		return nfsexportv1.Role_ROLE_STANDBY
	case RolePromoting:
		return nfsexportv1.Role_ROLE_PROMOTING
	case RoleActive:
		return nfsexportv1.Role_ROLE_ACTIVE
	}
	return nfsexportv1.Role_ROLE_UNSPECIFIED
}

func roleFromProto(r nfsexportv1.Role) Role {
	switch r {
	case nfsexportv1.Role_ROLE_STANDBY:
		return RoleStandby
	case nfsexportv1.Role_ROLE_PROMOTING:
		return RolePromoting
	case nfsexportv1.Role_ROLE_ACTIVE:
		return RoleActive
	}
	return 0
}

func outcomeToProto(o PromoteOutcome) nfsexportv1.PromoteOutcome {
	switch o {
	case PromoteActive:
		return nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_ACTIVE
	case PromoteCollision:
		return nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_COLLISION
	case PromoteSuperseded:
		return nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_SUPERSEDED
	case PromoteFenceFailed:
		return nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_FENCE_FAILED
	case PromoteStaleEpoch:
		return nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_STALE_EPOCH
	}
	return nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_UNSPECIFIED
}

func outcomeFromProto(o nfsexportv1.PromoteOutcome) PromoteOutcome {
	switch o {
	case nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_ACTIVE:
		return PromoteActive
	case nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_COLLISION:
		return PromoteCollision
	case nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_SUPERSEDED:
		return PromoteSuperseded
	case nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_FENCE_FAILED:
		return PromoteFenceFailed
	case nfsexportv1.PromoteOutcome_PROMOTE_OUTCOME_STALE_EPOCH:
		return PromoteStaleEpoch
	}
	return 0
}
