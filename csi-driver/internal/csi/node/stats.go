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

// redirectToActiveVolume is called when VolumeInfo returns ErrVolumeNotFound for
// the source volume, typically after a migration with --delete-source removed it.
// It queries the replication relationship on the source cluster (which survives
// volume deletion) to find the active volume on the target cluster, then fetches
// connection info from the target. Returns nil if redirection is not possible.
func (ns *Server) redirectToActiveVolume(
	ctx context.Context,
	srcClient controlplane.ClusterAPI,
	srcLvolID, volumeID string,
	vc map[string]string,
) map[string]string {
	rel, err := srcClient.GetRelationship(ctx, srcLvolID)
	if err != nil || rel == nil {
		klog.Warningf("replication relationship lookup failed for deleted volume %s: %v", volumeID, err)
		return nil
	}
	activeLvolID := rel.ActiveLvolID
	targetClusterID := rel.TargetClusterID
	targetPoolID := rel.TargetPoolID
	if activeLvolID == "" || targetClusterID == "" || targetPoolID == "" {
		klog.Warningf("relationship for %s has incomplete target info (cluster=%s pool=%s active=%s)",
			volumeID, targetClusterID, targetPoolID, activeLvolID)
		return nil
	}
	tgtClient, err := clusters.Client(ctx, targetClusterID, targetPoolID)
	if err != nil {
		klog.Warningf("target cluster %s not in secret file for deleted volume %s: %v",
			targetClusterID, volumeID, err)
		return nil
	}
	connInfo, err := tgtClient.VolumeInfo(ctx, activeLvolID, vc["hostNQN"])
	if err != nil {
		klog.Warningf("failed to fetch connection info from target cluster %s for volume %s: %v",
			targetClusterID, activeLvolID, err)
		return nil
	}
	klog.Infof("redirected deleted volume %s → active volume %s on cluster %s",
		volumeID, activeLvolID, targetClusterID)
	// Override cluster_id and poolID so the initiator uses the target cluster
	// for any subsequent API calls. Without this the initiator inherits the
	// source cluster_id from vc and fails looking up the target volume there.
	connInfo["cluster_id"] = targetClusterID
	connInfo["poolID"] = targetPoolID
	return connInfo
}
