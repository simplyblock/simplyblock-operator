// Creating a volume from something that already exists: another volume, or a
// snapshot of one.
package controller

import (
	"context"
	"fmt"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

func (cs *Server) handleVolumeContentSource(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	poolName string,
	vol *csi.Volume,
	sizeBytes int64,
) (*csi.Volume, error) {
	volumeSource := req.GetVolumeContentSource()
	switch volumeSource.GetType().(type) {
	case *csi.VolumeContentSource_Snapshot:
		return cs.handleSnapshotSource(ctx, volumeSource.GetSnapshot(), req, poolName, vol, sizeBytes)
	case *csi.VolumeContentSource_Volume:
		return cs.handleVolumeSource(ctx, volumeSource.GetVolume(), req, poolName, vol, sizeBytes)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "%v not a proper volume source", volumeSource)
	}
}

func (cs *Server) handleSnapshotSource(
	ctx context.Context,
	snapshot *csi.VolumeContentSource_SnapshotSource,
	req *csi.CreateVolumeRequest,
	poolName string,
	vol *csi.Volume,
	sizeBytes int64,
) (*csi.Volume, error) {
	if snapshot == nil {
		return nil, nil
	}
	csiSnapshotID := snapshot.GetSnapshotId()
	sbSnapshot, err := parseSnapshotID(csiSnapshotID)
	if err != nil {
		klog.Errorf("failed to get spdk snapshot, csiSnapshotID: %s err: %v", csiSnapshotID, err)
		return nil, status.Errorf(codes.NotFound, "snapshot %q not found: %v", csiSnapshotID, err)
	}
	// Use destination pool (from StorageClass params), not source snapshot pool.
	sbclient, err := clusters.Client(ctx, sbSnapshot.clusterID, poolName)
	if err != nil {
		klog.Errorf("failed to create spdk client: %v", err)
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	klog.Infof("CreateSnapshot : snapshotID=%s", sbSnapshot.snapshotID)
	snapshotName := req.GetName()
	params := req.GetParameters()
	pvcName, pvcNameSelected := params[csicommon.CSIStorageNameKey]
	pvcNamespace, pvcNamespaceSelected := params[csicommon.CSIStorageNamespaceKey]

	pvcFullName := pvcName
	consistencyGroup := ""
	if pvcNameSelected && pvcNamespaceSelected {
		pvcFullName = fmt.Sprintf("%s/%s", pvcNamespace, pvcName)
		// A restore PVC carrying the consistency-group label forms a NEW group
		// from the clones (design §7.2), so the label rides the clone body
		// exactly as it rides the create body (design §4.1).
		if _, pvcLabels, metaErr := cs.fetchPVCMeta(ctx, pvcName, pvcNamespace); metaErr == nil {
			consistencyGroup = pvcLabels[consistencyGroupLabel]
		} else {
			klog.Errorf("failed to read PVC %s/%s labels for the clone: %v", pvcNamespace, pvcName, metaErr)
		}
	}
	// Use raw bytes to avoid decimal/binary unit ambiguity in clone sizing.
	newSize := strconv.FormatInt(sizeBytes, 10)
	volumeID, err := sbclient.CloneSnapshot(ctx, sbSnapshot.snapshotID, snapshotName, newSize, pvcFullName, consistencyGroup)
	if err != nil {
		if !classifyCreateVolumeError(err).IsIdempotent() {
			klog.Errorf("error cloning snapshot: %v", err)
			return nil, err
		}
		// 409: a clone with this name already exists, so reconcile it.
		existingUUID, rerr := reconcileExistingVolume(ctx, sbclient, snapshotName, sizeBytes)
		if rerr != nil {
			return nil, rerr
		}
		if existingUUID != "" {
			vol.VolumeId = fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), existingUUID)
			return vol, nil
		}
		volumeID, err = sbclient.CloneSnapshot(ctx, sbSnapshot.snapshotID, snapshotName, newSize, pvcFullName, consistencyGroup)
		if err != nil {
			klog.Errorf("error re-cloning snapshot after cleanup: %v", err)
			return nil, err
		}
	}
	vol.VolumeId = fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), volumeID)
	klog.V(5).Info("successfully Restored Snapshot from Simplyblock with Volume ID: ", vol.GetVolumeId())

	return vol, nil
}

//nolint:unparam // poolName retained for call-site clarity
func (cs *Server) handleVolumeSource(
	ctx context.Context,
	srcVolume *csi.VolumeContentSource_VolumeSource,
	req *csi.CreateVolumeRequest,
	poolName string,
	vol *csi.Volume,
	sizeBytes int64,
) (*csi.Volume, error) {
	if srcVolume == nil {
		return nil, nil
	}
	srcVolumeID := srcVolume.GetVolumeId()

	klog.Infof("srcVolumeID=%s", srcVolumeID)

	cloneName := req.GetName()
	params := req.GetParameters()
	pvcName, pvcNameSelected := params[csicommon.CSIStorageNameKey]
	pvcNamespace, pvcNamespaceSelected := params[csicommon.CSIStorageNamespaceKey]
	pvcFullName := pvcName
	if pvcNameSelected && pvcNamespaceSelected {
		pvcFullName = fmt.Sprintf("%s/%s", pvcNamespace, pvcName)
	}

	spdkVol, err := csicommon.ParseVolumeHandle(srcVolumeID)
	if err != nil {
		klog.Errorf("failed to get spdk volume, srcVolumeID: %s err: %v", srcVolumeID, err)
		return nil, status.Errorf(codes.NotFound, "source volume %q not found: %v", srcVolumeID, err)
	}
	// Volume clone goes to the same pool as the source volume.
	sbclient, err := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef)

	if err != nil {
		klog.Errorf("failed to create spdk client: %v", err)
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	// Use raw bytes to avoid decimal/binary unit ambiguity in clone sizing.
	newSize := strconv.FormatInt(sizeBytes, 10)
	klog.Infof("CloneVolume : cloneName=%s", cloneName)
	volumeID, err := sbclient.CloneVolume(ctx, spdkVol.VolumeID, cloneName, newSize, pvcFullName)
	if err != nil {
		if !classifyCreateVolumeError(err).IsIdempotent() {
			klog.Errorf("error cloning volume: %v", err)
			return nil, err
		}
		// 409: a clone with this name already exists, so reconcile it.
		existingUUID, rerr := reconcileExistingVolume(ctx, sbclient, cloneName, sizeBytes)
		if rerr != nil {
			return nil, rerr
		}
		if existingUUID != "" {
			vol.VolumeId = fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), existingUUID)
			return vol, nil
		}
		volumeID, err = sbclient.CloneVolume(ctx, spdkVol.VolumeID, cloneName, newSize, pvcFullName)
		if err != nil {
			klog.Errorf("error re-cloning volume after cleanup: %v", err)
			return nil, err
		}
	}
	vol.VolumeId = fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), volumeID)
	klog.V(5).Info("successfully created clone volume from Simplyblock with Volume ID: ", vol.GetVolumeId())

	return vol, nil
}
