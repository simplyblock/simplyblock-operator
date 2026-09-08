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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/nqn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi-common"
	"github.com/simplyblock/csi-driver/internal/guardian"
	"github.com/simplyblock/csi-driver/internal/initiator"
	sbkube "github.com/simplyblock/csi-driver/internal/kubernetes"
	"github.com/simplyblock/csi-driver/internal/mount"
	"github.com/simplyblock/csi-driver/internal/reconnect"
)

type nodeServer struct {
	*csicommon.DefaultNodeServer
	// mounter performs the node-local half of staging: reading a device,
	// formatting and mounting it, and managing the path it is mounted on. It is
	// injectable so a test can script the probe's answers and observe exactly
	// which commands staging chose to run, which is how the never-format
	// contract is asserted.
	mounter     *mount.Mounter
	volumeLocks *csicommon.VolumeLocks
	kubeClient  kubernetes.Interface
	manager     *sbkube.Manager
	guardian    *guardian.Guardian
}

//nolint:unparam // error return kept for constructor symmetry / future use
func newNodeServer(d *csicommon.CSIDriver, kubeClient kubernetes.Interface) (*nodeServer, error) {
	ns := &nodeServer{
		DefaultNodeServer: csicommon.NewDefaultNodeServer(d),
		mounter:           mount.New(),
		volumeLocks:       csicommon.NewVolumeLocks(),
		kubeClient:        kubeClient,
	}

	// Build one Kubernetes cache manager and share it across the node plugin:
	// the reconnect loop reads PVs every ~3s and the guardian reads PVs/PVCs
	// per pod on every poll, so a single shared instance means a single PV
	// Watch and a single PVC Watch. The manager serves reads from cache once
	// synced and transparently falls back to the API until then (and if it
	// never syncs), so consumers need no fallback of their own.
	manager := sbkube.NewManager(ns.kubeClient)
	manager.Start(context.Background())
	ns.manager = manager

	nodeName := ns.Driver.GetNodeID()
	gcfg := guardian.NewDefaultConfig(nodeName)
	podGuardian, gerr := guardian.Start(context.Background(), gcfg, manager)
	if gerr != nil {
		klog.Errorf("failed to start guardian: %v", gerr)
	} else {
		ns.guardian = podGuardian
	}

	go reconnect.MonitorConnection(func(lvolID string) {
		if ns.guardian != nil {
			ns.guardian.MarkBrokenLvol(lvolID)
		}
	}, manager, ns.Driver.GetName(), nodeName)

	return ns, nil
}

func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	topology := ns.buildAccessibleTopology(ctx)

	response := &csi.NodeGetInfoResponse{
		NodeId: ns.Driver.GetNodeID(),
	}

	if len(topology) > 0 {
		response.AccessibleTopology = &csi.Topology{Segments: topology}
	}

	return response, nil
}

func (ns *nodeServer) buildAccessibleTopology(ctx context.Context) map[string]string {
	if ns.kubeClient == nil {
		return nil
	}

	nodeName := ns.Driver.GetNodeID()
	if nodeName == "" {
		return nil
	}

	const maxRetries = 5
	const retryDelay = 5 * time.Second

	node, err := ns.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	for attempt := 2; err != nil && attempt <= maxRetries; attempt++ {
		klog.Warningf("topology discovery: failed to get node %s (attempt %d/%d): %v",
			nodeName, attempt-1, maxRetries, err)
		time.Sleep(retryDelay)
		node, err = ns.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	}
	if err != nil {
		// All retries exhausted. Crash so the pod restarts and retries from a
		// clean state — registering without topology silently breaks PVC provisioning.
		klog.Fatalf("topology discovery: giving up after %d attempts for node %s — crashing to trigger pod restart: %v",
			maxRetries, nodeName, err)
	}

	segments := make(map[string]string)

	if zone, ok := node.Labels[topologyKeyZoneStable]; ok && zone != "" {
		segments[topologyKeyZoneStable] = zone
	} else if zone, ok := node.Labels[topologyKeyZoneBeta]; ok && zone != "" {
		segments[topologyKeyZoneStable] = zone
	}

	if region, ok := node.Labels[topologyKeyRegionStable]; ok && region != "" {
		segments[topologyKeyRegionStable] = region
	}

	for key, val := range node.Labels {
		if strings.HasPrefix(key, "simplyblock.io/pool.") && val == "allowed" {
			segments[key] = val
		}
		if strings.HasPrefix(key, topologyKeyStorageNodeUUIDPrefix) {
			segments[key] = val
		}
	}

	if len(segments) == 0 {
		// No zone/region labels found. Return hostname so the external-provisioner
		// can still build AccessibilityRequirements — without at least one topology
		// key on the CSINode, WaitForFirstConsumer provisioning fails. The controller
		// falls through to its single-cluster fallback when hostname doesn't match
		// any zone/region map entry.
		return map[string]string{"topology.simplyblock.io/hostname": node.Name}
	}

	return segments
}

func (ns *nodeServer) NodeGetVolumeStats(
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

		// Compute in uint64 (Bsize is int64 on Linux but uint32 on darwin; the block
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
// the source volume — typically after a migration with --delete-source removed it.
// It queries the replication relationship on the source cluster (which survives
// volume deletion) to find the active volume on the target cluster, then fetches
// connection info from the target. Returns nil if redirection is not possible.
func (ns *nodeServer) redirectToActiveVolume(
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
	// for any subsequent API calls — without this the initiator inherits the
	// source cluster_id from vc and fails looking up the target volume there.
	connInfo["cluster_id"] = targetClusterID
	connInfo["poolID"] = targetPoolID
	return connInfo
}

func (ns *nodeServer) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath() // use this directory to persistently store VolumeContext
	stagingTargetPath := getStagingTargetPath(req)

	isStaged, err := ns.mounter.IsMounted(stagingTargetPath)
	if err != nil {
		klog.Errorf("failed to check isStaged, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if isStaged {
		// A staged volume whose backing NVMe-oF device was lost leaves a dead
		// (EIO) mount that isStaged still reports as staged. Repair it in place
		// instead of short-circuiting.
		if !ns.mounter.IsDead(stagingTargetPath) {
			klog.Warning("volume already staged")
			return &csi.NodeStageVolumeResponse{}, nil
		}
		klog.Warningf("volume %s already staged but its mount is dead; restaging", volumeID)
		if err := ns.restageVolume(ctx, volumeID, stagingTargetPath, stagingParentPath, req.GetVolumeCapability()); err != nil { //nolint:lll // unwrappable string/log/signature
			return nil, status.Errorf(codes.Internal, "restage volume %s: %v", volumeID, err)
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}

	var nvmeInitiator initiator.Initiator
	vc := req.GetVolumeContext()

	vc["stagingParentPath"] = stagingParentPath

	if ns.kubeClient != nil {
		nodeName := ns.Driver.GetNodeID()
		node, nodeErr := ns.kubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if nodeErr == nil {
			vc["hostNQN"] = nqn.Host(string(node.UID))
		} else {
			klog.Warningf("failed to get node %s for hostNQN: %v", nodeName, nodeErr)
		}
	}

	if spdkVol, err := parseVolumeHandle(volumeID); err == nil {
		vc["poolID"] = spdkVol.PoolRef

		// Re-fetch connection info from the backend when:
		// - the volume was provisioned against a pool with allowed_hosts (nqn/targetType empty), or
		// - the volume may have been failed over (always refresh so the backend can redirect
		//   to the clone and return target_lvol_id for correct device lookup).
		if sbcClient, clientErr := clusters.Client(ctx, spdkVol.ClusterID, spdkVol.PoolRef); clientErr == nil {
			connInfo, infoErr := sbcClient.VolumeInfo(ctx, spdkVol.VolumeID, vc["hostNQN"])
			if infoErr != nil {
				if errors.Is(infoErr, controlplane.ErrVolumeNotFound) {
					// Source volume was deleted (migration with --delete-source).
					// Query the replication relationship to find the active volume
					// on the target cluster and redirect to it.
					connInfo = ns.redirectToActiveVolume(ctx, sbcClient, spdkVol.VolumeID, volumeID, vc)
				}
				if connInfo == nil {
					klog.Warningf("failed to fetch volume connection info for %s: %v", volumeID, infoErr)
				}
			}
			for k, v := range connInfo {
				vc[k] = v
			}
		}
	}

	nvmeInitiator, err = initiator.New(vc)
	if err != nil {
		klog.Errorf("failed to create spdk initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	devicePath, err := nvmeInitiator.Connect(ctx) // idempotent
	if err != nil {
		klog.Errorf("failed to connect initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer func() {
		if err != nil {
			nvmeInitiator.Disconnect(ctx) //nolint:errcheck // ignore error
		}
	}()
	if err = ns.stageVolume(ctx, devicePath, stagingTargetPath, req, vc); err != nil { // idempotent
		klog.Errorf("failed to stage volume, volumeID: %s devicePath:%s err: %v", volumeID, devicePath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	vc["devicePath"] = devicePath
	// stash VolumeContext to stagingParentPath (useful during Unstage as it has no
	// VolumeContext passed to the RPC as per the CSI spec)
	err = stashVolumeContext(req.GetVolumeContext(), stagingParentPath)
	if err != nil {
		klog.Errorf("failed to stash volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath()
	stagingTargetPath := getStagingTargetPath(req)

	err := ns.mounter.Remove(stagingTargetPath) // idempotent
	if err != nil {
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Errorf(codes.Internal, "unstage volume %s failed: %s", volumeID, err)
	}

	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		klog.Errorf("failed to lookup volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	nvmeInitiator, err := initiator.New(volumeContext)
	if err != nil {
		klog.Errorf("failed to create spdk initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cleanupCancel()
	err = nvmeInitiator.Disconnect(cleanupCtx) // idempotent
	if err != nil {
		klog.Errorf("failed to disconnect initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := cleanUpVolumeContext(stagingParentPath); err != nil {
		klog.Errorf("failed to clean up volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	// If the backing NVMe-oF device was lost (total path loss), repair it before
	// bind-mounting into the pod — otherwise the pod inherits the dead mount/
	// missing device. kubelet skips NodeStage when the volume is still referenced
	// on this node (e.g., a same-node pod replacement), so NodePublish is the
	// reliable place to heal.
	if err := ns.healVolumeBeforePublish(ctx, req); err != nil {
		klog.Errorf("failed to heal volume %s before publish: %v", volumeID, err)
		return nil, status.Errorf(codes.Internal, "heal volume %s before publish: %v", volumeID, err)
	}

	err := ns.publishVolume(getStagingTargetPath(req), req) // idempotent
	if err != nil {
		klog.Errorf("failed to publish volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	if ns.guardian != nil {
		ns.guardian.RegisterPublish(req.VolumeContext[paramClusterID], req.VolumeContext["uuid"], req.TargetPath)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	err := ns.mounter.Remove(req.GetTargetPath()) // idempotent
	if err != nil {
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", req.GetTargetPath(), err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	if ns.guardian != nil {
		if spdkVol, err := parseVolumeHandle(volumeID); err == nil {
			ns.guardian.RegisterUnpublish(spdkVol.VolumeID, req.TargetPath)
		} else {
			klog.Warningf("NodeUnpublishVolume: could not parse volume ID %q for Guardian tracking: %v", volumeID, err)
		}
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeGetCapabilities(
	_ context.Context,
	_ *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_VOLUME_CONDITION,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
		},
	}, nil
}

func (ns *nodeServer) NodeExpandVolume(
	ctx context.Context,
	req *csi.NodeExpandVolumeRequest,
) (*csi.NodeExpandVolumeResponse, error) {
	klog.Infof("NodeExpandVolume: called with args %+v", req)

	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	volumeMountPath := req.GetVolumePath()

	stagingParentPath := req.GetStagingTargetPath()
	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve volume context for volume %s: %v", volumeID, err)
	}

	devicePath, ok := volumeContext["devicePath"]
	if !ok || devicePath == "" {
		return nil, status.Errorf(codes.Internal, "could not find device path for volume %s", volumeID)
	}

	// For raw block volumes, the block device has already been resized at the
	// storage layer. Skipping filesystem resize is correct here because:
	// - resize2fs (ext4) can operate on an unmounted raw device, so it worked accidentally
	// - xfs_growfs requires a mounted filesystem path and cannot operate on raw block devices
	// Neither tool should be invoked for block volumes.
	if cap := req.GetVolumeCapability(); cap != nil && cap.GetBlock() != nil {
		klog.Infof("NodeExpandVolume: volume %s is a block device, skipping filesystem resize", volumeID)
		return &csi.NodeExpandVolumeResponse{}, nil
	}

	needsResize, err := ns.mounter.NeedsResize(devicePath, volumeMountPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if volume %s needs resizing: %v", volumeID, err)
	}

	if needsResize {
		resized, err := ns.mounter.Resize(devicePath, volumeMountPath)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to resize volume %s: %v", volumeID, err)
		}
		if resized {
			klog.Infof(
				"Successfully resized volume %s (device: %s, mount path: %s)",
				volumeID,
				devicePath,
				volumeMountPath,
			)
		} else {
			klog.Warningf("Volume %s did not require resizing", volumeID)
		}
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

// must be idempotent
//
//nolint:cyclop // many cases in switch increases complexity
func (ns *nodeServer) stageVolume(
	ctx context.Context,
	devicePath, stagingPath string,
	req *csi.NodeStageVolumeRequest,
	volumeContext map[string]string,
) error {
	if req.GetVolumeCapability().GetBlock() != nil {
		klog.Infof(
			"NodeStageVolume: called for volume %s. Skipping staging since it is a block device.",
			req.GetVolumeId(),
		)
		return nil
	}

	mounted, err := ns.mounter.EnsureDirectory(stagingPath)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	fsType := fsTypeOrDefault(req.GetVolumeCapability())

	// Read the device before deciding anything about it. Staging stops here on
	// any reading that is not a definite answer, rather than handing an
	// uncertain device to mkfs.
	fs, err := ns.mounter.Probe(ctx, devicePath)
	if err != nil {
		return err
	}

	// The probe found a filesystem: the format question is settled, and the
	// device is mounted directly rather than through SafeFormatAndMount. That
	// helper probes the device again itself and formats whenever its own probe
	// reads blank, so on a fabric that degrades between the two probes it
	// reformats a filesystem staging just positively identified — and the
	// annotation guard below never runs on this path, because it is only
	// consulted when the preflight reads blank. The helper also preen-repairs
	// (fsck -a) every existing filesystem it mounts read-write, which writes to
	// a device whose path state staging cannot judge.
	//
	// A filesystem that is not the one asked for stops staging here.
	//
	// The volume was formatted once and holds data, so the class saying something
	// else now is somebody having changed what a class says about a volume that
	// already exists. Neither way of reconciling that is safe. Reformatting
	// destroys the volume, which is the failure this path exists to prevent.
	// Mounting the one that is there works, and leaves a volume serving a
	// filesystem nobody declared, with the disagreement in a log line and nowhere
	// else, until whatever notices next decides to make the device match the
	// class again — and that decision reformats.
	//
	// Refusing costs an outage on a volume nobody can currently mount correctly
	// anyway, and it puts the misconfiguration in front of an operator while the
	// data is still there.
	if fs != "" {
		if fs != fsType {
			return status.Errorf(codes.FailedPrecondition,
				"volume %s carries a %s filesystem and its class asks for %s; refusing to stage it, "+
					"because reformatting would destroy the volume and mounting it as %s would serve "+
					"a filesystem the class does not declare",
				req.GetVolumeId(), fs, fsType, fs)
		}
		volumeContext[stagedFsTypeKey] = fs
		if err := ns.mounter.Mount(
			devicePath,
			stagingPath,
			fs,
			stagingMountFlags(fs, req.GetVolumeCapability()),
		); err != nil {
			return err
		}
		ns.recordOnDiskFilesystem(ctx, req.GetVolumeId(), volumeContext, fs)
		return nil
	}

	// blkid reporting nothing means either that the device carries no filesystem
	// or that it could not be read, and the two are indistinguishable from its
	// exit code alone. A claim that records a filesystem settles it: the volume
	// was formatted once, so this reading is a failed probe rather than a blank
	// device, and it is mounted as what the claim says is down there.
	//
	// A record that is not the filesystem asked for stops staging, for the reason
	// the branch above stops: the volume was formatted once and holds data, so a
	// class naming something else cannot be reconciled here. Reformatting
	// destroys it, and mounting the recorded one serves a filesystem nobody
	// declared until something later decides to correct the mismatch.
	//
	// A mount that fails here is the correct outcome and must stay one. It means
	// the claim's record disagrees with the device, or the device is genuinely
	// dead, and neither is a reason to format: falling back to mkfs would
	// reinstate the data loss this branch exists to prevent.
	mntFlags := stagingMountFlags(fsType, req.GetVolumeCapability())
	annotated, err := ns.annotatedFilesystem(ctx, req.GetVolumeId(), volumeContext)
	if err != nil {
		return err
	}
	if annotated != "" {
		if annotated != fsType {
			return status.Errorf(codes.FailedPrecondition,
				"volume %s is recorded as holding a %s filesystem and its class asks for %s; refusing to "+
					"stage it, because reformatting would destroy the volume and mounting it as %s would "+
					"serve a filesystem the class does not declare",
				req.GetVolumeId(), annotated, fsType, annotated)
		}
		volumeContext[stagedFsTypeKey] = annotated
		return ns.mounter.Mount(
			devicePath,
			stagingPath,
			annotated,
			stagingMountFlags(annotated, req.GetVolumeCapability()),
		)
	}

	// Record what was actually staged: a later restage remounts an existing
	// filesystem, and the volume capability alone no longer answers which one it
	// is once the annotation has overridden it.
	volumeContext[stagedFsTypeKey] = fsType

	formatOptions := mount.FormatOptions(fsType, volumeContext)

	klog.Infof("mount %s to %s, fstype: %s, flags: %v", devicePath, stagingPath, fsType, mntFlags)
	klog.Infof("formatOptions %v", formatOptions)
	if err := ns.mounter.FormatAndMount(devicePath, stagingPath, fsType, mntFlags, formatOptions); err != nil {
		return err
	}

	if fsType == "ext4" {
		if err := mount.ApplyExt4Reserved(devicePath, volumeContext["tune2fs_reserved_blocks"]); err != nil {
			return err
		}
	}

	// The device now definitely carries fsType: mkfs just put it there — a device
	// already carrying a filesystem returned from the probe-found branch above.
	// Record that on the claim, so what a volume is formatted with is answerable
	// without a node to run blkid on.
	ns.recordOnDiskFilesystem(ctx, req.GetVolumeId(), volumeContext, fsType)

	return nil
}

// stagedFsType returns the filesystem a volume was staged with: the one
// recorded at stage time when it is there, and otherwise the one the volume
// capability asks for, which is all a volume staged by an older driver has.
func stagedFsType(volumeContext map[string]string, volCap *csi.VolumeCapability) string {
	if fsType := strings.TrimSpace(volumeContext[stagedFsTypeKey]); fsType != "" {
		return fsType
	}
	return fsTypeOrDefault(volCap)
}

const (
	// annotationOnDiskFilesystem, set on a PersistentVolumeClaim, names the
	// filesystem to put on that claim's volume. It overrides the filesystem the
	// StorageClass asks for, which is what makes a single class usable by
	// workloads that disagree about the filesystem they want.
	annotationOnDiskFilesystem = "storage.simplyblock.io/on-disk-filesystem"

	// stagedFsTypeKey is the volume-context key under which the filesystem a
	// volume was staged with is recorded, alongside the other node-local keys
	// stashed at the staging path.
	stagedFsTypeKey = "stagedFsType"
)

// persistentVolumeClaimForVolume returns the PersistentVolumeClaim that owns the
// given CSI volume.
//
// The claim's namespace and name usually travel in the volume context: the
// external-provisioner runs with --extra-create-metadata, so CreateVolume was
// told which claim it was provisioning for and copied that into the context
// that became the PersistentVolume's volume attributes. A volume without them —
// provisioned by an older driver, or by a hand-written PersistentVolume — is
// resolved the long way instead: find the PersistentVolume carrying this volume
// handle, and follow its claim reference.
func (ns *nodeServer) persistentVolumeClaimForVolume(
	ctx context.Context,
	volumeID string,
	volumeContext map[string]string,
) (*corev1.PersistentVolumeClaim, error) {
	namespace := strings.TrimSpace(volumeContext[CSIStorageNamespaceKey])
	name := strings.TrimSpace(volumeContext[CSIStorageNameKey])

	if namespace == "" || name == "" {
		spdkVol, err := parseVolumeHandle(volumeID)
		if err != nil {
			return nil, fmt.Errorf("resolve claim for volume %s: %w", volumeID, err)
		}

		pv, err := ns.manager.PersistentVolumeByLogicalVolumeID(ctx, spdkVol.VolumeID)
		if err != nil {
			return nil, fmt.Errorf("resolve claim for volume %s: %w", volumeID, err)
		}
		if pv.Spec.ClaimRef == nil {
			return nil, fmt.Errorf(
				"resolve claim for volume %s: persistent volume %s is not bound to a claim",
				volumeID, pv.Name,
			)
		}
		namespace, name = pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
	}

	return ns.manager.PersistentVolumeClaimByNamespaceAndName(ctx, namespace, name)
}

// annotatedFilesystem returns the filesystem the volume's claim asks to have put
// on disk, and the empty string when the claim asks for none: that is the one
// reading under which staging carries on with the filesystem the volume
// capability names, exactly as it did before the annotation existed.
//
// It errors on the other two readings. A claim that cannot be read leaves the
// blank probe that led here unexplained, and a claim asking for a filesystem
// this driver does not create is an instruction that cannot be carried out;
// under either one, whether the device holds data is still open, so staging
// fails rather than formatting through the doubt.
func (ns *nodeServer) annotatedFilesystem(
	ctx context.Context,
	volumeID string,
	volumeContext map[string]string,
) (string, error) {
	pvc, err := ns.persistentVolumeClaimForVolume(ctx, volumeID, volumeContext)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("volume %s: no claim to read %s from: %v", volumeID, annotationOnDiskFilesystem, err)
		} else {
			klog.Warningf("volume %s: failed to read %s from its claim: %v", volumeID, annotationOnDiskFilesystem, err)
		}
		return "", err
	}

	fsType := strings.ToLower(strings.TrimSpace(pvc.Annotations[annotationOnDiskFilesystem]))
	if fsType == "" {
		return "", nil
	}
	if !mount.Supported(fsType) {
		klog.Warningf(
			"claim %s/%s asks for on-disk filesystem %q, which this driver does not create; refusing to stage it",
			pvc.Namespace, pvc.Name, fsType,
		)
		return "", errors.New("unsupported filesystem type")
	}
	return fsType, nil
}

// recordOnDiskFilesystem writes the filesystem a volume was staged with onto its
// claim, under the same annotation that requests one. The annotation is a
// request only while the device is blank; from the first successful stage on it
// is the record of what is actually down there, which is why it is written back
// rather than left as whatever was asked for.
//
// It writes only when the claim does not already say this, so a volume that is
// staged on every pod start costs one write in total rather than one per start.
// Nothing here can fail staging: the volume is formatted and mounted by the time
// this runs, and a claim that cannot be read or written is a lost note, not a
// broken mount.
func (ns *nodeServer) recordOnDiskFilesystem(
	ctx context.Context,
	volumeID string,
	volumeContext map[string]string,
	fsType string,
) {
	if ns.kubeClient == nil || fsType == "" {
		return
	}

	pvc, err := ns.persistentVolumeClaimForVolume(ctx, volumeID, volumeContext)
	if err != nil {
		klog.Warningf("volume %s: no claim to record the on-disk filesystem on: %v", volumeID, err)
		return
	}
	if pvc.Annotations[annotationOnDiskFilesystem] == fsType {
		return
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{annotationOnDiskFilesystem: fsType},
		},
	})
	if err != nil {
		klog.Warningf("volume %s: failed to build the %s patch: %v", volumeID, annotationOnDiskFilesystem, err)
		return
	}

	// A merge patch of the one key, so a concurrent writer of any other
	// annotation on this claim is left alone.
	if _, err := ns.kubeClient.CoreV1().PersistentVolumeClaims(pvc.Namespace).Patch(
		ctx, pvc.Name, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		klog.Warningf(
			"volume %s: failed to record %s=%s on claim %s/%s: %v",
			volumeID, annotationOnDiskFilesystem, fsType, pvc.Namespace, pvc.Name, err,
		)
		return
	}
	klog.Infof("volume %s: recorded %s=%s on claim %s/%s",
		volumeID, annotationOnDiskFilesystem, fsType, pvc.Namespace, pvc.Name)
}

// fsTypeOrDefault returns the requested filesystem type, defaulting to ext4.
func fsTypeOrDefault(volCap *csi.VolumeCapability) string {
	if fsType := volCap.GetMount().GetFsType(); fsType != "" {
		return fsType
	}
	return "ext4"
}

// stagingMountFlags builds the mount flags used when mounting a volume at its
// staging path, so the initial stage and a later restage stay consistent. It
// takes the filesystem actually on disk rather than reading it off volCap,
// because a claim annotation can have overridden what the capability asked for.
func stagingMountFlags(fsType string, volCap *csi.VolumeCapability) []string {
	flags := append([]string{}, volCap.GetMount().GetMountFlags()...)
	flags = append(flags, mount.FlagsFor(fsType)...)

	switch volCap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		flags = append(flags, "ro")
	case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_UNKNOWN:
	}
	return flags
}

// healVolumeBeforePublish repairs a volume whose backing NVMe-oF device was lost
// (total path loss) before it is bind-mounted into a (replacement) pod. For
// filesystem volumes it restages the dead staging mount; for block volumes it
// reconnects the missing device. No-op when the volume is healthy.
func (ns *nodeServer) healVolumeBeforePublish(ctx context.Context, req *csi.NodePublishVolumeRequest) error {
	volCap := req.GetVolumeCapability()
	stagingParentPath := req.GetStagingTargetPath()

	switch {
	case volCap.GetBlock() != nil:
		return ns.ensureDeviceConnected(ctx, req.GetVolumeId(), stagingParentPath)
	case volCap.GetMount() != nil:
		stagingTargetPath := getStagingTargetPath(req)
		if ns.mounter.IsDead(stagingTargetPath) {
			return ns.restageVolume(ctx, req.GetVolumeId(), stagingTargetPath, stagingParentPath, volCap)
		}
	}
	return nil
}

// ensureDeviceConnected reconnects a block volume's NVMe-oF device if it has
// gone away. The by-id device path is stable across reconnects, so only the
// connection needs re-establishing (no mount). Idempotent.
func (ns *nodeServer) ensureDeviceConnected(ctx context.Context, volumeID, stagingParentPath string) error {
	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		return fmt.Errorf("lookup volume context: %w", err)
	}
	if devicePath := volumeContext["devicePath"]; devicePath != "" && deviceExists(devicePath) {
		return nil
	}

	klog.Warningf("block volume %s device is gone; reconnecting NVMe-oF", volumeID)
	nvmeInitiator, err := initiator.New(volumeContext)
	if err != nil {
		return fmt.Errorf("new initiator: %w", err)
	}
	devicePath, err := nvmeInitiator.Connect(ctx) // idempotent
	if err != nil {
		return fmt.Errorf("reconnect device: %w", err)
	}
	if volumeContext["devicePath"] != devicePath {
		volumeContext["devicePath"] = devicePath
		if err := stashVolumeContext(volumeContext, stagingParentPath); err != nil {
			klog.Warningf("ensureDeviceConnected: re-stash volume context for %s: %v", volumeID, err)
		}
	}
	klog.Infof("reconnected block volume %s device %s", volumeID, devicePath)
	return nil
}

// deviceExists reports whether path resolves to an existing device, following
// symlinks such as /dev/disk/by-id/nvme-<uuid>_ha_1.
func deviceExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// restageVolume repairs a staging mount whose backing NVMe-oF device was lost
// (total path loss → the kernel removed the device, leaving a dead EIO mount).
// It force-unmounts the dead mount, reconnects the volume, and remounts the
// EXISTING filesystem in place. It never reformats — the volume already holds
// data. Filesystem (mount) volumes only; block volumes have no staging mount.
func (ns *nodeServer) restageVolume(
	ctx context.Context,
	volumeID, stagingTargetPath, stagingParentPath string,
	volCap *csi.VolumeCapability,
) error {
	if volCap.GetMount() == nil {
		klog.Warningf("restageVolume: volume %s is not a filesystem volume; skipping", volumeID)
		return nil
	}
	klog.Warningf(
		"restaging volume %s: staging mount %s is dead, reconnecting NVMe-oF and remounting",
		volumeID,
		stagingTargetPath,
	)

	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		return fmt.Errorf("lookup volume context: %w", err)
	}

	if err := ns.mounter.ForceUnmount(stagingTargetPath); err != nil {
		return fmt.Errorf("unmount dead staging mount: %w", err)
	}

	nvmeInitiator, err := initiator.New(volumeContext)
	if err != nil {
		return fmt.Errorf("new initiator: %w", err)
	}
	devicePath, err := nvmeInitiator.Connect(ctx) // idempotent: re-establishes the lost device
	if err != nil {
		return fmt.Errorf("reconnect device: %w", err)
	}

	if _, err := ns.mounter.EnsureDirectory(stagingTargetPath); err != nil {
		return fmt.Errorf("recreate staging dir: %w", err)
	}
	// Plain Mount, not FormatAndMount: the volume already holds a filesystem and
	// reformatting would destroy data.
	fsType := stagedFsType(volumeContext, volCap)
	if err := ns.mounter.Mount(devicePath, stagingTargetPath, fsType, stagingMountFlags(fsType, volCap)); err != nil {
		return fmt.Errorf("remount device %s at %s: %w", devicePath, stagingTargetPath, err)
	}

	volumeContext["devicePath"] = devicePath
	if err := stashVolumeContext(volumeContext, stagingParentPath); err != nil {
		klog.Warningf("restageVolume: failed to re-stash volume context for %s: %v", volumeID, err)
	}
	klog.Infof("restaged volume %s on fresh device %s", volumeID, devicePath)
	return nil
}

// must be idempotent
func (ns *nodeServer) publishVolume(stagingPath string, req *csi.NodePublishVolumeRequest) error {
	targetPath := req.GetTargetPath()

	fsType := req.GetVolumeCapability().GetMount().GetFsType()

	if req.GetVolumeCapability().GetBlock() != nil {
		stagingParentPath := req.GetStagingTargetPath()
		volumeContext, err := lookupVolumeContext(stagingParentPath)
		if err != nil {
			return status.Errorf(
				codes.Internal,
				"failed to retrieve volume context for volume %s: %v",
				req.GetVolumeId(),
				err,
			)
		}

		devicePath, ok := volumeContext["devicePath"]
		if !ok || devicePath == "" {
			return status.Errorf(codes.Internal, "could not find device path for volume %s", req.GetVolumeId())
		}
		stagingPath = devicePath

		fsType = ""

		if err := ns.mounter.EnsureCleanTarget(targetPath); err != nil {
			return status.Errorf(codes.Internal, "Could not cleanup mount target %q: %v", targetPath, err)
		}

		if err = ns.mounter.EnsureFile(targetPath); err != nil {
			if removeErr := os.Remove(targetPath); removeErr != nil {
				return status.Errorf(codes.Internal, "Could not remove mount target %q: %v", targetPath, removeErr)
			}
			return status.Errorf(codes.Internal, "Could not create file %q: %v", targetPath, err)
		}
	} else if req.GetVolumeCapability().GetMount() != nil {
		mounted, err := ns.mounter.EnsureDirectory(targetPath)
		if err != nil {
			return err
		}
		if mounted {
			return nil
		}
	}

	mntFlags := req.GetVolumeCapability().GetMount().GetMountFlags()
	mntFlags = append(mntFlags, "bind")
	klog.Infof("mount %s to %s, fstype: %s, flags: %v", stagingPath, targetPath, fsType, mntFlags)
	return ns.mounter.Mount(stagingPath, targetPath, fsType, mntFlags)
}

func getStagingTargetPath(req interface{}) string {
	switch vr := req.(type) {
	case *csi.NodeStageVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodeUnstageVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodePublishVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	default:
		klog.Warningf("invalid request %T", vr)
	}
	return ""
}
