// CreateVolume through a class that names a supercluster in cluster_id
// (design-superclusters.md §7.2, §9.2).
//
// A supercluster is no entry of the driver's secret: it has no backend cluster
// and no control plane of its own, only members, each of which is an entry. So
// a cluster_id the secret does not carry is asked about over the link: the
// operator, which holds the members' readings and records the answer on the
// claim, names the member, and the volume is created there as it would be for
// a class naming that member.
package controller

import (
	"context"
	"slices"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/placement/placementrpc"

	"github.com/simplyblock/csi-driver/internal/clusters"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// Placer asks the operator which member of a supercluster a volume goes to.
type Placer interface {
	Place(ctx context.Context, req placementrpc.Request) (string, error)
}

// SetPlacer gives the server the operator's placement service, reached over
// the link. Without one, a class naming a supercluster fails with Unavailable.
func (cs *Server) SetPlacer(p Placer) { cs.placer = p }

// inSecret reports whether the driver's secret carries the cluster. A secret
// that cannot be read answers yes, so the lookup that follows fails with the
// read error it always has.
func inSecret(clusterID string) bool {
	ids, err := clusters.List()
	if err != nil {
		return true
	}
	return slices.Contains(ids, clusterID)
}

// placeInSupercluster asks the operator for the member a volume of a class
// naming aggregateID is created in. NotFound means the identifier names no
// supercluster either, which is the class's mistake, and is InvalidArgument as
// an unknown cluster is. Every other refusal passes through with its code.
func (cs *Server) placeInSupercluster(
	ctx context.Context, req *csi.CreateVolumeRequest, aggregateID string,
) (*clusterSelection, error) {
	if cs.placer == nil {
		return nil, status.Errorf(codes.Unavailable,
			"cluster_id %s names no cluster in the driver's secret, and without the link to the operator "+
				"the driver cannot ask whether it names a supercluster", aggregateID)
	}
	params := req.GetParameters()
	placeReq := placementrpc.Request{
		AggregateID:     aggregateID,
		PoolName:        params["pool_name"],
		VolumeName:      req.GetName(),
		SizeBytes:       req.GetCapacityRange().GetRequiredBytes(),
		Preferred:       segmentsOf(req.GetAccessibilityRequirements().GetPreferred()),
		Requisite:       segmentsOf(req.GetAccessibilityRequirements().GetRequisite()),
		ClaimName:       params[csicommon.CSIStorageNameKey],
		ClaimNamespace:  params[csicommon.CSIStorageNamespaceKey],
		SourceClusterID: sourceCluster(req.GetVolumeContentSource()),
	}
	if placeReq.ClaimName != "" && placeReq.ClaimNamespace != "" && cs.kubeClient != nil {
		if _, labels, err := cs.fetchPVCMeta(ctx, placeReq.ClaimName, placeReq.ClaimNamespace); err == nil {
			placeReq.ConsistencyGroup = labels[consistencyGroupLabel]
		}
	}

	member, err := cs.placer.Place(ctx, placeReq)
	if err != nil {
		klog.Errorf("placement of volume %s in supercluster %s failed: %v", req.GetName(), aggregateID, err)
		s, isStatus := status.FromError(err)
		switch {
		case !isStatus:
			return nil, status.Errorf(codes.Unavailable, "ask the operator where volume %s goes: %v", req.GetName(), err)
		case s.Code() == codes.NotFound:
			return nil, status.Errorf(codes.InvalidArgument,
				"cluster_id %s names no cluster in the driver's secret and no supercluster: %s", aggregateID, s.Message())
		}
		return nil, err
	}
	if member == "" {
		return nil, status.Errorf(codes.Internal, "the operator placed volume %s in no member", req.GetName())
	}
	return &clusterSelection{clusterID: member}, nil
}

// segmentsOf is the segment sets of a CSI topology list.
func segmentsOf(topologies []*csi.Topology) []map[string]string {
	if len(topologies) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(topologies))
	for _, t := range topologies {
		out = append(out, t.GetSegments())
	}
	return out
}

// sourceCluster is the backend cluster of a clone's or restore's source, read
// from its handle, or empty for a new volume or a handle that does not parse.
func sourceCluster(source *csi.VolumeContentSource) string {
	switch {
	case source.GetSnapshot() != nil:
		if snapshot, err := parseSnapshotID(source.GetSnapshot().GetSnapshotId()); err == nil {
			return snapshot.clusterID
		}
	case source.GetVolume() != nil:
		if handle, err := csicommon.ParseVolumeHandle(source.GetVolume().GetVolumeId()); err == nil {
			return handle.ClusterID
		}
	}
	return ""
}
