// Snapshots: creating, deleting, and listing them, and the identifier they are
// addressed by, which is not quite a volume handle.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

type spdkSnapshot struct {
	clusterID  string
	poolID     string
	snapshotID string
}

func parseSnapshotID(csiSnapshotID string) (*spdkSnapshot, error) {
	ids := strings.Split(csiSnapshotID, ":")
	switch len(ids) {
	case 3:
		// New 3-part format: {clusterID}:{poolID}:{snapshotID}
		return &spdkSnapshot{clusterID: ids[0], poolID: ids[1], snapshotID: ids[2]}, nil
	case 2:
		// Legacy 2-part format: {clusterID}:{snapshotID}, with the pool resolved at delete time
		return &spdkSnapshot{clusterID: ids[0], snapshotID: ids[1]}, nil
	default:
		return nil, fmt.Errorf("invalid snapshot ID format: %s", csiSnapshotID)
	}
}

// reconcileExistingSnapshot handles a 409 from CreateSnapshot: the control plane
// says a snapshot with this name already exists. It lists snapshots and, if the
// existing one has the same source volume, returns it as success (CSI
// idempotency, since this is the driver's own snapshot from an earlier attempt).
// If the source
// differs, it is a real name conflict → AlreadyExists.
func reconcileExistingSnapshot(
	ctx context.Context,
	sbclient controlplane.ClusterAPI,
	sourceLvolID, snapshotName string,
) (*csi.CreateSnapshotResponse, error) {
	snaps, err := sbclient.ListSnapshots(ctx)
	if err != nil {
		return nil, classifyCreateSnapshotError(err)
	}
	for _, s := range snaps {
		if s.Name != snapshotName {
			continue
		}
		if lvolIDFromURL(s.LvolURL) != sourceLvolID {
			return nil, status.Errorf(
				codes.AlreadyExists,
				"snapshot %q already exists with a different source volume",
				snapshotName,
			)
		}
		creationTime := timestamppb.Now()
		if ts, perr := time.Parse(time.RFC3339Nano, s.CreatedAt); perr == nil {
			creationTime = timestamppb.New(ts)
		}
		return &csi.CreateSnapshotResponse{
			Snapshot: &csi.Snapshot{
				SizeBytes:      s.Size,
				SnapshotId:     fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), s.UUID),
				SourceVolumeId: sourceLvolID,
				CreationTime:   creationTime,
				ReadyToUse:     true,
			},
		}, nil
	}
	return nil, status.Errorf(codes.Internal, "snapshot %q reported as existing but was not found", snapshotName)
}

func (cs *Server) CreateSnapshot(
	ctx context.Context,
	req *csi.CreateSnapshotRequest,
) (*csi.CreateSnapshotResponse, error) {
	volumeID := req.GetSourceVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "source volume ID is required")
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
	}

	klog.Infof("CreateSnapshot : volumeID=%s", volumeID)
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	snapshotName := req.GetName()
	klog.Infof("CreateSnapshot : snapshotName=%s", snapshotName)
	spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		klog.Errorf("failed to get spdk volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Errorf(codes.InvalidArgument, "invalid source volume ID %q: %v", volumeID, err)
	}
	sbclient, err := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef)
	if err != nil {
		klog.Errorf("failed to create spdk client: %v", err)
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	volSize, err := sbclient.GetVolumeSize(ctx, spdkVol.VolumeID)
	klog.Infof("CreateSnapshot : volSize=%s", volSize)
	if err != nil {
		klog.Errorf("failed to get volume info, volumeID: %s err: %v", volumeID, err)
		return nil, classifyCreateSnapshotError(err)
	}
	size, err := strconv.ParseInt(volSize, 10, 64)
	if err != nil {
		klog.Errorf("failed to parse volume size, size: %s err: %v", volSize, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	snapshotID, err := sbclient.CreateSnapshot(ctx, spdkVol.VolumeID, snapshotName)
	klog.Infof("CreateSnapshot : snapshotID=%s", snapshotID)
	if err != nil {
		d := classifyCreateSnapshotError(err)
		if d.IsIdempotent() {
			// 409: the snapshot already exists. Reconcile: if it is this driver's (same
			// source) return it as success (CSI idempotency), and if it belongs to a
			// different source, it is a genuine name conflict.
			return reconcileExistingSnapshot(ctx, sbclient, spdkVol.VolumeID, snapshotName)
		}
		klog.Errorf("failed to create snapshot, volumeID: %s snapshotName: %s err: %v", volumeID, snapshotName, err)
		return nil, d
	}

	creationTime := timestamppb.Now()
	snapshotData := csi.Snapshot{
		SizeBytes:      size,
		SnapshotId:     snapshotID,
		SourceVolumeId: spdkVol.VolumeID,
		CreationTime:   creationTime,
		ReadyToUse:     true,
	}

	return &csi.CreateSnapshotResponse{
		Snapshot: &snapshotData,
	}, nil
}

func (cs *Server) DeleteSnapshot(
	ctx context.Context,
	req *csi.DeleteSnapshotRequest,
) (*csi.DeleteSnapshotResponse, error) {
	csiSnapshotID := req.GetSnapshotId()
	if csiSnapshotID == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot ID is required")
	}

	unlock := cs.volumeLocks.Lock(csiSnapshotID)
	defer unlock()

	sbSnapshot, err := parseSnapshotID(csiSnapshotID)
	if err != nil {
		// Invalid format means the snapshot was never created by this driver, so treat
		// it as already deleted.
		klog.Warningf("invalid snapshot ID format, treating as already deleted: %s", csiSnapshotID)
		return &csi.DeleteSnapshotResponse{}, nil
	}
	sbclient, err := clusters.Client(ctx, sbSnapshot.clusterID, sbSnapshot.poolID)
	if err != nil {
		if errors.Is(err, controlplane.ErrClusterNotFound) {
			// The cluster this snapshot lived on has been removed from management.
			// The snapshot is unreachable and effectively gone, so report success and let the
			// external-snapshotter drops its finalizer instead of retrying forever.
			klog.Warningf("cluster for snapshot %s no longer managed, treating as already deleted: %v", csiSnapshotID, err)
			return &csi.DeleteSnapshotResponse{}, nil
		}
		klog.Errorf("failed to create spdk client: %v", err)
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	klog.Infof("Deleting Snapshot : csiSnapshotID=%s sbSnapshotID=%s", csiSnapshotID, sbSnapshot.snapshotID)

	err = sbclient.DeleteSnapshot(ctx, sbSnapshot.snapshotID)
	if err != nil {
		if d := classifyDeleteSnapshotError(err); !d.IsSuccess() {
			klog.Errorf("failed to delete snapshot, snapshotID: %s err: %v", csiSnapshotID, err)
			return nil, d
		}
		// already gone: idempotent success
		klog.Warningf("snapshot not found, treating as already deleted: %s", csiSnapshotID)
	}

	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots lists all snapshots across all clusters
func (cs *Server) ListSnapshots(
	ctx context.Context,
	req *csi.ListSnapshotsRequest,
) (*csi.ListSnapshotsResponse, error) {

	var entries []*controlplane.SnapshotResp
	clusterIDs, err := clusters.List()
	if err != nil {
		return nil, err
	}

	for _, clusterID := range clusterIDs {
		sbclient, err := clusters.Client(ctx, clusterID, "")
		if err != nil {
			klog.Errorf("failed to create spdk client: %v", err)
			return nil, status.Error(codes.Unavailable, err.Error())
		}

		snapshotEntries, err := sbclient.ListSnapshots(ctx)
		if err != nil {
			return nil, classifyListSnapshotsError(err)
		}
		entries = append(entries, snapshotEntries...)
	}

	var all []*csi.ListSnapshotsResponse_Entry
	for _, entry := range entries {
		snapshotID := fmt.Sprintf("%s:%s:%s", entry.ClusterID, entry.PoolID, entry.UUID)
		sourceVolumeID := lvolIDFromURL(entry.LvolURL)

		if req.GetSnapshotId() != "" && req.GetSnapshotId() != snapshotID {
			continue
		}
		if req.GetSourceVolumeId() != "" && req.GetSourceVolumeId() != sourceVolumeID {
			continue
		}

		createdAt, _ := time.Parse(time.RFC3339Nano, entry.CreatedAt)
		all = append(all, &csi.ListSnapshotsResponse_Entry{
			Snapshot: &csi.Snapshot{
				SizeBytes:      entry.Size,
				SnapshotId:     snapshotID,
				SourceVolumeId: sourceVolumeID,
				CreationTime:   timestamppb.New(createdAt),
				ReadyToUse:     true,
			},
		})
	}

	page, nextToken, err := paginateSnapshots(all, req.GetStartingToken(), int(req.GetMaxEntries()))
	if err != nil {
		return nil, err
	}

	return &csi.ListSnapshotsResponse{
		Entries:   page,
		NextToken: nextToken,
	}, nil
}

// paginateSnapshots returns one page of entries starting at startingToken (an
// absolute index from a prior call). pageSize 0 returns all remaining entries.
func paginateSnapshots(
	all []*csi.ListSnapshotsResponse_Entry,
	startingToken string,
	pageSize int,
) ([]*csi.ListSnapshotsResponse_Entry, string, error) {
	start := 0
	if startingToken != "" {
		var parseErr error
		start, parseErr = strconv.Atoi(startingToken)
		if parseErr != nil || start < 0 {
			return nil, "", status.Errorf(codes.Aborted, "invalid starting token: %q", startingToken)
		}
	}
	if start > len(all) {
		start = len(all)
	}
	page := all[start:]

	var nextToken string
	if pageSize > 0 && len(page) > pageSize {
		nextToken = strconv.Itoa(start + pageSize)
		page = page[:pageSize]
	}

	return page, nextToken, nil
}

// lvolIDFromURL extracts the volume UUID from a URL path like
// /api/v2/clusters/{id}/storage-pools/{id}/volumes/{volume_id}
func lvolIDFromURL(lvolURL string) string {
	u := strings.TrimRight(lvolURL, "/")
	if idx := strings.LastIndex(u, "/"); idx >= 0 {
		return u[idx+1:]
	}
	return lvolURL
}
