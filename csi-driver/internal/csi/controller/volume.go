// Creating and deleting a volume, and the idempotency each RPC owes: the CSI
// spec lets kubelet retry any of them, so every one has to reconcile against a
// volume that may already exist.
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

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/kube"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// CreateVolume creates a new volume in the SimplyBlock storage system.
func (cs *Server) CreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	volumeID := req.GetName()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	selection, err := cs.resolveClusterSelection(req)
	if err != nil {
		klog.Errorf("failed to resolve cluster selection for volume %s: %v", volumeID, err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	poolName := req.GetParameters()["pool_name"]
	sbClient, err := clusters.Client(ctx, selection.clusterID, poolName)
	if err != nil {
		return nil, err
	}

	csiVolume, err := cs.createVolume(ctx, req, sbClient)
	if err != nil {
		klog.Errorf("failed to create volume, volumeID: %s err: %v", volumeID, err)
		if _, isStatus := status.FromError(err); isStatus {
			return nil, err
		}
		return nil, classifyCreateVolumeError(err)
	}

	volumeInfo, err := cs.publishVolume(ctx, csiVolume.GetVolumeId(), sbClient)
	if err != nil {
		klog.Errorf("failed to publish volume, volumeID: %s err: %v", volumeID, err)
		return nil, classifyCreateVolumeError(err)
	}

	// copy volume info. node needs these info to contact target(ip, port, nqn, ...)
	if csiVolume.VolumeContext == nil {
		csiVolume.VolumeContext = volumeInfo
	} else {
		for k, v := range volumeInfo {
			csiVolume.VolumeContext[k] = v
		}
	}

	if csiVolume.VolumeContext == nil {
		csiVolume.VolumeContext = map[string]string{}
	}
	csiVolume.VolumeContext[csicommon.ParamClusterID] = selection.clusterID

	if volType, ok := req.GetParameters()["type"]; ok {
		csiVolume.VolumeContext["targetType"] = volType
	}

	// Merge in DHCHAP's allowed-node segment so its PV gets nodeAffinity too
	// (issue #403) — resolveClusterSelection only tracks zone/region.
	topologySegments := copyTopologySegments(selection.topology)
	if key, val := dhchapAllowedNodeSegment(req); key != "" {
		if topologySegments == nil {
			topologySegments = map[string]string{}
		}
		topologySegments[key] = val
	}

	if len(topologySegments) > 0 {
		csiVolume.AccessibleTopology = []*csi.Topology{{Segments: topologySegments}}
	}
	// selection.topology is nil for a StorageClass that selects its cluster
	// directly via cluster_id (no zone/region routing configured) — the
	// DHCHAP-gated case #403 added support for above. zoneFromSegments/
	// regionFromSegments nil-check internally, so this stays a no-op rather
	// than needing a guard here too.
	if zone := zoneFromSegments(selection.topology); zone != "" {
		csiVolume.VolumeContext[csicommon.TopologyKeyZoneStable] = zone
	}
	if region := regionFromSegments(selection.topology); region != "" {
		csiVolume.VolumeContext[csicommon.TopologyKeyRegionStable] = region
	}

	// placement-hint is a one-shot creation-time hint. Now that the volume is fully
	// created and published on the requested node, clear it so it does not linger
	// or get mistaken for a hard pin. selected-storage-node (a persistent pin) and
	// the legacy host-id (owned by pre-existing PVCs) are deliberately left
	// untouched. Best-effort: a failure to clear the hint must never fail
	// provisioning (and, since this runs only on the success path, a retried
	// CreateVolume still sees the hint if publish failed).
	params := req.GetParameters()
	pvcName, pvcNamespace := params[csicommon.CSIStorageNameKey], params[csicommon.CSIStorageNamespaceKey]
	if pvcName != "" && pvcNamespace != "" {
		if rerr := cs.removePVCAnnotations(ctx, pvcName, pvcNamespace, kube.AnnoPlacementHint); rerr != nil {
			klog.Warningf("createVolume: could not clear placement-hint on PVC %s/%s: %v", pvcNamespace, pvcName, rerr)
		}
	}

	return &csi.CreateVolumeResponse{Volume: csiVolume}, nil
}

func (cs *Server) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest,
) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	// Invalid format means the volume was never created by this driver - treat as already deleted.
	if _, err := csicommon.ParseVolumeHandle(volumeID); err != nil {
		klog.Warningf("invalid volume ID format, treating as already deleted: %s", volumeID)
		return &csi.DeleteVolumeResponse{}, nil
	}

	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()
	// no harm if volume already unpublished
	err := cs.unpublishVolume(ctx, volumeID)
	switch {
	case errors.Is(err, controlplane.ErrVolumeUnpublished):
		klog.Warningf("volume not published: %s", volumeID)
	case errors.Is(err, controlplane.ErrClusterNotFound):
		// The cluster this volume lived on has been removed from management. The
		// volume is unreachable and effectively gone; report success so the
		// external-provisioner drops its finalizer instead of retrying forever.
		klog.Warningf("cluster for volume %s no longer managed, treating as already deleted: %v", volumeID, err)
		return &csi.DeleteVolumeResponse{}, nil
	case err != nil:
		klog.Errorf("failed to unpublish volume, volumeID: %s err: %v", volumeID, err)
		return nil, classifyDeleteVolumeError(err)
	}

	// no harm if volume already deleted
	err = cs.deleteVolume(ctx, volumeID)
	switch {
	case errors.Is(err, controlplane.ErrVolumeNotFound):
		// deleted in previous request?
		klog.Warningf("volume not exists: %s", volumeID)
	case errors.Is(err, controlplane.ErrClusterNotFound):
		// The cluster this volume lived on has been removed from management (e.g.
		// its secret config changed between unpublish and delete). The volume is
		// unreachable and effectively gone; report success so the
		// external-provisioner drops its finalizer instead of retrying forever.
		klog.Warningf("cluster for volume %s no longer managed, treating as already deleted: %v", volumeID, err)
	case err != nil:
		klog.Errorf("failed to delete volume, volumeID: %s err: %v", volumeID, err)
		return nil, classifyDeleteVolumeError(err)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *Server) prepareCreateVolumeReq(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	capacityBytes int64,
) (*controlplane.CreateLVolData, bool, error) {
	params := req.GetParameters()

	maxNamespace, err := kube.IntParam(params, "max_namespace_per_subsys", 1)
	if err != nil {
		return nil, false, err
	}

	encryption, err := kube.BoolParam(params, "encryption", false)
	if err != nil {
		return nil, false, err
	}

	pvcName, pvcNameSelected := params[csicommon.CSIStorageNameKey]
	pvcNamespace, pvcNamespaceSelected := params[csicommon.CSIStorageNamespaceKey]

	pvcFullName := pvcName
	if pvcNameSelected && pvcNamespaceSelected {
		pvcFullName = fmt.Sprintf("%s/%s", pvcNamespace, pvcName)
	}

	var pvcAnns map[string]string
	if pvcNameSelected && pvcNamespaceSelected {
		pvcAnns, err = cs.fetchPVCAnnotations(ctx, pvcName, pvcNamespace)
		if err != nil {
			return nil, false, err
		}
	}

	// host_id priority: selected-storage-node (hard pin) → placement-hint (one-shot
	// hint from the placement webhook) → host-id and its deprecated form (legacy
	// fallback for pre-existing PVCs).
	hostID := pvcAnnotation(pvcAnns,
		kube.AnnoSelectedStorageNode, kube.AnnoPlacementHint, kube.AnnoHostID, kube.DeprecatedAnnoHostID)
	lvolID := pvcAnnotation(pvcAnns, annotationLvolID, deprecatedAnnotationLvolID)
	podAffinitive, _ := strconv.ParseBool(pvcAnns[annotationPodAffinity])

	// QoS from StorageClass, overridable per-PVC via annotations.
	maxRWIOPS := params["qos_rw_iops"]
	maxRWmBytes := params["qos_rw_mbytes"]
	maxRmBytes := params["qos_r_mbytes"]
	maxWmBytes := params["qos_w_mbytes"]
	if pvcNameSelected && pvcNamespaceSelected {
		if v := pvcAnnotation(pvcAnns, annotationQoSRWIOPS, deprecatedAnnotationQoSRWIOPS); v != "" {
			maxRWIOPS = v
		}
		if v := pvcAnnotation(pvcAnns, annotationQoSRWMBps, deprecatedAnnotationQoSRWMBps); v != "" {
			maxRWmBytes = v
		}
		if v := pvcAnnotation(pvcAnns, annotationQoSRMBps, deprecatedAnnotationQoSRMBps); v != "" {
			maxRmBytes = v
		}
		if v := pvcAnnotation(pvcAnns, annotationQoSWMBps, deprecatedAnnotationQoSWMBps); v != "" {
			maxWmBytes = v
		}
	}

	createVolReq := controlplane.CreateLVolData{
		LvolName:     req.GetName(),
		Size:         strconv.FormatInt(capacityBytes, 10),
		LvsName:      params["pool_name"],
		Fabric:       params["fabric"],
		MaxRWIOPS:    maxRWIOPS,
		MaxRWmBytes:  maxRWmBytes,
		MaxRmBytes:   maxRmBytes,
		MaxWmBytes:   maxWmBytes,
		MaxSize:      params["max_size"],
		MaxNamespace: maxNamespace,
		Encryption:   encryption,
		HostID:       hostID,
		LvolID:       lvolID,
		Namespaced:   maxNamespace > 1,
		PvcName:      pvcFullName,
	}
	return &createVolReq, podAffinitive, nil
}

// reconcileExistingVolume handles a 409 (name already exists) on a volume create
// or clone. If an online volume with the name exists it is reused (after a size
// check); a non-online leftover from a failed earlier attempt is deleted so the
// caller can recreate. It returns the existing volume's UUID to reuse, "" to
// recreate, or an error (a size conflict, or a list/delete failure).
func reconcileExistingVolume(
	ctx context.Context,
	sbclient controlplane.ClusterAPI,
	name string,
	requiredBytes int64,
) (string, error) {
	volumes, err := sbclient.ListVolumes(ctx)
	if err != nil {
		return "", err
	}
	for _, v := range volumes {
		if v.Name != name {
			continue
		}
		if strings.EqualFold(v.Status, "online") {
			if requiredBytes > 0 {
				aligned := alignToGiBBytes(requiredBytes)
				if v.LvolSize != aligned {
					return "", status.Errorf(
						codes.AlreadyExists,
						"volume %q exists with size %d but requested %d",
						name,
						v.LvolSize,
						aligned,
					)
				}
			}
			klog.Infof("reconcile: reusing online existing volume %q id=%s", name, v.UUID)
			return v.UUID, nil
		}
		// Non-online leftover from a failed attempt: it will never come online.
		klog.Warningf("reconcile: deleting non-online leftover volume %q id=%s status=%s", name, v.UUID, v.Status)
		if delErr := sbclient.DeleteVolume(ctx, v.UUID); delErr != nil {
			return "", delErr
		}
	}
	return "", nil
}

func (cs *Server) createVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	sbclient controlplane.ClusterAPI,
) (*csi.Volume, error) {
	size := req.GetCapacityRange().GetRequiredBytes()
	if size == 0 {
		klog.Warningln("invalid volume size, resize to 1G")
		size = 1024 * 1024 * 1024
	}

	capacityBytes := alignToGiBBytes(size)
	vol := csi.Volume{
		CapacityBytes: capacityBytes,
		VolumeContext: req.GetParameters(),
		ContentSource: req.GetVolumeContentSource(),
	}

	klog.V(5).Info("provisioning volume from SDK node..")
	poolName := req.GetParameters()["pool_name"]
	if req.GetVolumeContentSource() != nil {
		clonedVolume, clonedErr := cs.handleVolumeContentSource(ctx, req, poolName, &vol, capacityBytes)
		if clonedErr != nil {
			return nil, clonedErr
		}
		if clonedVolume != nil {
			return clonedVolume, nil
		}
	}

	createVolReq, podAffinitive, err := cs.prepareCreateVolumeReq(ctx, req, capacityBytes)
	if err != nil {
		return nil, err
	}

	// Co-locate the volume's primary with the consuming Pod's scheduled worker
	// (node-affinity placement) when no explicit host_id was already set from a
	// PVC annotation — an explicit annotation is a deliberate override and wins
	// — and only when the PVC opted in via simplyblock.io/pod-affinity: "true".
	// Without that annotation, Tier 1 is skipped for this PVC even if the Pod's
	// resolved node hosts a co-located storage node.
	if podAffinitive && createVolReq.HostID == "" {
		if coLocated := coLocatedHostID(req.GetAccessibilityRequirements(), sbclient.ClusterID()); coLocated != "" {
			klog.Infof("createVolume: co-locating volume %s with storage node %s from pod scheduling topology",
				req.GetName(), coLocated)
			createVolReq.HostID = coLocated
		}
	}

	// Store the effective QoS values into VolumeContext so the PV spec records
	// what was actually applied.
	vol.VolumeContext["qos_rw_iops"] = createVolReq.MaxRWIOPS
	vol.VolumeContext["qos_rw_mbytes"] = createVolReq.MaxRWmBytes
	vol.VolumeContext["qos_r_mbytes"] = createVolReq.MaxRmBytes
	vol.VolumeContext["qos_w_mbytes"] = createVolReq.MaxWmBytes

	volumeID, err := sbclient.CreateVolume(ctx, createVolReq)
	if err != nil {
		if errors.Is(err, controlplane.ErrVolumeExists) {
			klog.Infof("createVolume: volume %q already exists, reconciling", req.GetName())
			existingUUID, rerr := reconcileExistingVolume(
				ctx,
				sbclient,
				req.GetName(),
				req.GetCapacityRange().GetRequiredBytes(),
			)
			if rerr != nil {
				return nil, rerr
			}
			if existingUUID != "" {
				vol.VolumeId = fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), existingUUID)
				return &vol, nil
			}
			// The non-online leftover (if any) has been cleaned up; create fresh.
			volumeID, err = sbclient.CreateVolume(ctx, createVolReq)
			if err != nil {
				klog.Errorf("createVolume: recreate after cleanup failed: %v", err)
				return nil, err
			}
			vol.VolumeId = fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), volumeID)
			return &vol, nil
		}
		klog.Errorf("error creating simplyBlock volume: %v", err)
		return nil, err
	}
	vol.VolumeId = fmt.Sprintf("%s:%s:%s", sbclient.ClusterID(), sbclient.PoolID(), volumeID)
	klog.V(5).Info("successfully created volume from Simplyblock with Volume ID: ", vol.GetVolumeId())

	return &vol, nil
}

func (cs *Server) publishVolume(
	ctx context.Context,
	volumeID string,
	sbclient controlplane.ClusterAPI,
) (map[string]string, error) {
	spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		return nil, err
	}
	err = sbclient.PublishVolume(ctx, spdkVol.VolumeID)
	if err != nil {
		return nil, err
	}

	// hostNQN is not available in the controller path; pass empty string.
	// If the volume has allowed_hosts configured, this call will fail and the
	// node will re-fetch connection info at NodeStageVolume time using its own NQN.
	volumeInfo, err := sbclient.VolumeInfo(ctx, spdkVol.VolumeID, "")
	if err != nil {
		klog.Warningf("failed to get volume info for %s (will be fetched at stage time): %v", spdkVol.VolumeID, err)
		return map[string]string{}, nil
	}
	return volumeInfo, nil
}

func (cs *Server) deleteVolume(ctx context.Context, volumeID string) error {
	spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		return err
	}
	sbclient, err := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef)
	if err != nil {
		return err
	}
	return sbclient.DeleteVolume(ctx, spdkVol.VolumeID)
}

func (cs *Server) unpublishVolume(ctx context.Context, volumeID string) error {
	spdkVol, err := csicommon.ParseVolumeHandle(volumeID)
	if err != nil {
		return err
	}
	sbclient, err := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef)
	if err != nil {
		return err
	}
	return sbclient.UnpublishVolume(ctx, spdkVol.VolumeID)
}
