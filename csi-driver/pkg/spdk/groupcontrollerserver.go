/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spdk

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog"

	"github.com/spdk/spdk-csi/pkg/util"
)

// groupControllerServer implements csi.GroupControllerServer.
// It provides multi-volume consistent snapshots (VolumeGroupSnapshot) backed
// by the Simplyblock group-snapshot API at
// /api/v2/clusters/{id}/snapshot-groups/.
type groupControllerServer struct {
	csi.UnimplementedGroupControllerServer
	volumeLocks *util.VolumeLocks
}

func newGroupControllerServer() *groupControllerServer {
	return &groupControllerServer{
		volumeLocks: util.NewVolumeLocks(),
	}
}

// GroupControllerGetCapabilities advertises the group-snapshot capability.
func (gs *groupControllerServer) GroupControllerGetCapabilities(
	_ context.Context,
	_ *csi.GroupControllerGetCapabilitiesRequest,
) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	return &csi.GroupControllerGetCapabilitiesResponse{
		Capabilities: []*csi.GroupControllerServiceCapability{
			{
				Type: &csi.GroupControllerServiceCapability_Rpc{
					Rpc: &csi.GroupControllerServiceCapability_RPC{
						Type: csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT,
					},
				},
			},
		},
	}, nil
}

// CreateVolumeGroupSnapshot creates a consistent group snapshot across all
// provided source volumes. The group snapshot ID format is
// "{clusterID}:{groupSnapshotUUID}" so the delete path can identify the
// cluster without an additional lookup.
func (gs *groupControllerServer) CreateVolumeGroupSnapshot(
	ctx context.Context,
	req *csi.CreateVolumeGroupSnapshotRequest,
) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
	}
	sourceVolumeIDs := req.GetSourceVolumeIds()
	if len(sourceVolumeIDs) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one source volume ID is required")
	}

	// All source volumes must belong to the same cluster. Parse and validate.
	var clusterID, poolID string
	lvolIDs := make([]string, 0, len(sourceVolumeIDs))
	for _, volID := range sourceVolumeIDs {
		parsed, err := parseVolumeID(volID)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid source volume ID %q: %v", volID, err)
		}
		if clusterID == "" {
			clusterID = parsed.clusterID
			poolID = parsed.poolID
		} else if parsed.clusterID != clusterID {
			return nil, status.Error(codes.InvalidArgument, "all source volumes must belong to the same cluster")
		}
		lvolIDs = append(lvolIDs, parsed.lvolID)
	}

	// Acquire locks in sorted order to avoid deadlocks when multiple concurrent
	// CreateVolumeGroupSnapshot calls share overlapping volume sets.
	sorted := make([]string, len(sourceVolumeIDs))
	copy(sorted, sourceVolumeIDs)
	sortStrings(sorted)
	unlocks := make([]func(), 0, len(sorted))
	for _, id := range sorted {
		unlocks = append(unlocks, gs.volumeLocks.Lock(id))
	}
	defer func() {
		for _, u := range unlocks {
			u()
		}
	}()

	sbclient, err := util.NewsimplyBlockClient(ctx, clusterID, poolID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	groupResp, err := sbclient.CreateVolumeGroupSnapshot(ctx, lvolIDs, name)
	if err != nil {
		if isIdempotentGroupSnapshot(err) {
			return reconcileExistingGroupSnapshot(ctx, sbclient, clusterID, poolID, name, sourceVolumeIDs)
		}
		klog.Errorf("CreateVolumeGroupSnapshot failed: name=%s err=%v", name, err)
		return nil, status.Errorf(codes.Internal, "failed to create volume group snapshot: %v", err)
	}

	return buildGroupSnapshotResponse(clusterID, poolID, groupResp), nil
}

// DeleteVolumeGroupSnapshot deletes a group snapshot and all its member
// snapshots. It is idempotent: if the group snapshot is not found the call
// returns success so the external-snapshotter can remove its finalizer.
func (gs *groupControllerServer) DeleteVolumeGroupSnapshot(
	ctx context.Context,
	req *csi.DeleteVolumeGroupSnapshotRequest,
) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	groupCSIID := req.GetGroupSnapshotId()
	if groupCSIID == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot ID is required")
	}

	clusterID, groupID, err := parseGroupSnapshotID(groupCSIID)
	if err != nil {
		// Malformed ID — the snapshot was never created by this driver.
		klog.Warningf("invalid group snapshot ID format, treating as already deleted: %s", groupCSIID)
		return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
	}

	sbclient, err := util.NewsimplyBlockClient(ctx, clusterID, "")
	if err != nil {
		if isClusterNotFound(err) {
			klog.Warningf("cluster for group snapshot %s no longer managed, treating as already deleted", groupCSIID)
			return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	if err := sbclient.DeleteVolumeGroupSnapshot(ctx, groupID); err != nil {
		klog.Errorf("DeleteVolumeGroupSnapshot failed: id=%s err=%v", groupCSIID, err)
		return nil, status.Errorf(codes.Internal, "failed to delete volume group snapshot: %v", err)
	}

	return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}

// GetVolumeGroupSnapshot returns the current state of a group snapshot.
func (gs *groupControllerServer) GetVolumeGroupSnapshot(
	ctx context.Context,
	req *csi.GetVolumeGroupSnapshotRequest,
) (*csi.GetVolumeGroupSnapshotResponse, error) {
	groupCSIID := req.GetGroupSnapshotId()
	if groupCSIID == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot ID is required")
	}

	clusterID, groupID, err := parseGroupSnapshotID(groupCSIID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "group snapshot %q not found", groupCSIID)
	}

	// poolID is not encoded in the group snapshot ID; pass empty string.
	sbclient, err := util.NewsimplyBlockClient(ctx, clusterID, "")
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	groupResp, err := sbclient.GetVolumeGroupSnapshot(ctx, groupID)
	if err != nil {
		klog.Errorf("GetVolumeGroupSnapshot failed: id=%s err=%v", groupCSIID, err)
		return nil, status.Errorf(codes.NotFound, "group snapshot %q not found: %v", groupCSIID, err)
	}

	resp := buildGroupSnapshotResponse(clusterID, "", groupResp)
	return &csi.GetVolumeGroupSnapshotResponse{
		GroupSnapshot: resp.GroupSnapshot,
	}, nil
}

// --- helpers ---

// parseGroupSnapshotID splits a CSI group snapshot ID of the form
// "{clusterID}:{groupSnapshotUUID}" into its two components.
func parseGroupSnapshotID(csiID string) (clusterID, groupID string, err error) {
	parts := strings.SplitN(csiID, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected <clusterID>:<groupID>, got %q", csiID)
	}
	return parts[0], parts[1], nil
}

func isIdempotentGroupSnapshot(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

func isClusterNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cluster not found")
}

// sortStrings sorts a slice of strings in-place (stdlib sort would import
// "sort", which is already available; using a simple insertion sort here
// avoids the extra import for a typically small slice).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// buildGroupSnapshotResponse converts a backend GroupSnapshotResp into a CSI
// CreateVolumeGroupSnapshotResponse. poolID may be empty for GET responses.
func buildGroupSnapshotResponse(clusterID, poolID string, g *util.GroupSnapshotResp) *csi.CreateVolumeGroupSnapshotResponse {
	groupCSIID := fmt.Sprintf("%s:%s", clusterID, g.UUID)

	snapshots := make([]*csi.Snapshot, 0, len(g.Members))
	for _, m := range g.Members {
		snapCSIID := fmt.Sprintf("%s:%s:%s", clusterID, poolID, m.SnapshotID)
		snapshots = append(snapshots, &csi.Snapshot{
			SnapshotId:     snapCSIID,
			SourceVolumeId: m.VolumeID,
			ReadyToUse:     true,
			CreationTime:   timestamppb.New(parseCreatedAt(g.CreatedAt)),
		})
	}

	return &csi.CreateVolumeGroupSnapshotResponse{
		GroupSnapshot: &csi.VolumeGroupSnapshot{
			GroupSnapshotId: groupCSIID,
			Snapshots:       snapshots,
			CreationTime:    timestamppb.New(parseCreatedAt(g.CreatedAt)),
			ReadyToUse:      true,
		},
	}
}

func parseCreatedAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now()
	}
	return t
}

// reconcileExistingGroupSnapshot handles the 409-conflict idempotency case:
// a group snapshot with this name already exists. Fetch it and verify it
// matches the requested source volumes; return it if it does, or a
// AlreadyExists error if it conflicts.
func reconcileExistingGroupSnapshot(
	ctx context.Context,
	sbclient *util.ClusterClient,
	clusterID, poolID, name string,
	sourceVolumeIDs []string,
) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	groups, err := sbclient.ListVolumeGroupSnapshots(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list group snapshots for idempotency check: %v", err)
	}
	for _, g := range groups {
		if g.Name != name {
			continue
		}
		// Verify the existing group covers the same source volumes.
		if groupMatchesSources(g, sourceVolumeIDs) {
			klog.Infof("CreateVolumeGroupSnapshot: idempotent — returning existing group snapshot %s", g.UUID)
			return buildGroupSnapshotResponse(clusterID, poolID, g), nil
		}
		return nil, status.Errorf(codes.AlreadyExists,
			"group snapshot %q already exists with different source volumes", name)
	}
	return nil, status.Errorf(codes.Internal, "group snapshot %q reported as existing but not found", name)
}

func groupMatchesSources(g *util.GroupSnapshotResp, sourceVolumeIDs []string) bool {
	if len(g.Members) != len(sourceVolumeIDs) {
		return false
	}
	memberSet := make(map[string]struct{}, len(g.Members))
	for _, m := range g.Members {
		memberSet[m.VolumeID] = struct{}{}
	}
	for _, id := range sourceVolumeIDs {
		// sourceVolumeIDs are CSI volume IDs; extract the lvolID part.
		parsed, err := parseVolumeID(id)
		if err != nil {
			return false
		}
		if _, ok := memberSet[parsed.lvolID]; !ok {
			return false
		}
	}
	return true
}
