// Capacity and health reporting for a staged volume, including the redirect a
// failed-over volume needs before either can be read.
package node

import (
	"context"
	"os"

	"golang.org/x/sys/unix"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/mount"
)

func (ns *Server) NodeGetVolumeStats(
	ctx context.Context,
	req *csi.NodeGetVolumeStatsRequest,
) (*csi.NodeGetVolumeStatsResponse, error) {
	volID := req.GetVolumeId()
	volumePath := req.GetVolumePath()

	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_path is required")
	}

	st, err := os.Stat(volumePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Error(codes.NotFound, "volume_path not found")
		}
		return nil, status.Errorf(codes.Internal, "stat volume_path %q: %v", volumePath, err)
	}

	if st.IsDir() {
		var s unix.Statfs_t
		if err := unix.Statfs(volumePath, &s); err != nil {
			return nil, status.Errorf(codes.Internal, "statfs %q: %v", volumePath, err)
		}

		// Compute in uint64 (Bsize is int64 on Linux but uint32 on darwin, and the block
		// counts are uint64 on both) and convert the product once, so neither conversion
		// is a platform-dependent no-op.
		totalBytes := int64(s.Blocks * uint64(s.Bsize))
		availBytes := int64(s.Bavail * uint64(s.Bsize))
		usedBytes := totalBytes - availBytes
		if usedBytes < 0 {
			usedBytes = 0
		}

		totalInodes := int64(s.Files)
		availInodes := int64(s.Ffree)
		usedInodes := totalInodes - availInodes
		if usedInodes < 0 {
			usedInodes = 0
		}

		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				{
					Unit:      csi.VolumeUsage_BYTES,
					Total:     totalBytes,
					Used:      usedBytes,
					Available: availBytes,
				},
				{
					Unit:      csi.VolumeUsage_INODES,
					Total:     totalInodes,
					Used:      usedInodes,
					Available: availInodes,
				},
			},
		}, nil
	}

	sizeBytes, err := mount.BlockSizeBytes(volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get block size for %q: %v", volumePath, err)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Total:     int64(sizeBytes),
				Used:      0,
				Available: int64(sizeBytes),
			},
		},
	}, nil
}

// clusterClientFor resolves a cluster and pool to a control-plane client. A
// package variable, so redirectToActiveVolume's chain walk is testable with
// fake clients instead of a live secret file.
var clusterClientFor = func(ctx context.Context, clusterID, poolID string) (controlplane.ClusterAPI, error) {
	return clusters.Client(ctx, clusterID, poolID)
}

// redirectToActiveVolume is called when VolumeInfo returns ErrVolumeNotFound for
// the source volume, typically after a migration with --delete-source removed it
// or after a fail-over retired it. It follows the replication relationship
// (which survives volume deletion) to the volume actually serving the data and
// fetches connection info from there. Returns nil if redirection is not possible.
//
// The walk follows CHAINED pairings hop by hop: a relocate round trip leaves
// original -> hop-1 clone -> hop-2 clone, where only the last hop is live
// (each record names it as active_lvol_id, resolved transitively by the
// backend). Each hop uses that record's own target triple -- its cluster,
// pool, and lvol are consistent with EACH OTHER, while active_lvol_id may
// live on an entirely different cluster than the record's target fields
// describe. Pairing the first record's active_lvol_id with its target
// cluster asked cluster B for a volume living on cluster A, fell back to a
// stale stashed context, and timed the mount out (confirmed live 2026-09-24,
// relocate M-02's round trip).
func redirectToActiveVolume(
	ctx context.Context,
	srcClient controlplane.ClusterAPI,
	srcLvolID, volumeID string,
	vc map[string]string,
) map[string]string {
	client, lvolID := srcClient, srcLvolID
	for range 8 { // one hop per past fail-over; capped far above any real chain
		rel, err := client.GetRelationship(ctx, lvolID)
		if err != nil || rel == nil {
			klog.Warningf("replication relationship lookup failed for deleted volume %s (at hop %s): %v",
				volumeID, lvolID, err)
			return nil
		}
		if rel.TargetLvolID == "" || rel.TargetClusterID == "" || rel.TargetPoolID == "" {
			klog.Warningf("relationship for %s has incomplete target info (cluster=%s pool=%s lvol=%s)",
				volumeID, rel.TargetClusterID, rel.TargetPoolID, rel.TargetLvolID)
			return nil
		}
		tgtClient, err := clusterClientFor(ctx, rel.TargetClusterID, rel.TargetPoolID)
		if err != nil {
			klog.Warningf("target cluster %s not in secret file for deleted volume %s: %v",
				rel.TargetClusterID, volumeID, err)
			return nil
		}
		if rel.ActiveLvolID != "" && rel.ActiveLvolID != rel.TargetLvolID {
			// This pairing's target was itself superseded by a later
			// fail-over; keep walking from it toward the active volume.
			client, lvolID = tgtClient, rel.TargetLvolID
			continue
		}
		connInfo, err := tgtClient.VolumeInfo(ctx, rel.TargetLvolID, vc["hostNQN"])
		if err != nil {
			klog.Warningf("failed to fetch connection info from target cluster %s for volume %s: %v",
				rel.TargetClusterID, rel.TargetLvolID, err)
			return nil
		}
		klog.Infof("redirected deleted volume %s → active volume %s on cluster %s",
			volumeID, rel.TargetLvolID, rel.TargetClusterID)
		// Override cluster_id and poolID so the initiator uses the active
		// volume's cluster for any subsequent API calls. Without this the
		// initiator inherits the source cluster_id from vc and fails looking
		// up the volume there.
		connInfo[csicommon.ParamClusterID] = rel.TargetClusterID
		connInfo["poolID"] = rel.TargetPoolID
		return connInfo
	}
	klog.Warningf("replication chain for deleted volume %s did not converge within 8 hops", volumeID)
	return nil
}
