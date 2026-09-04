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
	osexec "os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/errs/deferrers"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	mount "k8s.io/mount-utils"
	"k8s.io/utils/exec"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	csicommon "github.com/spdk/spdk-csi/pkg/csi-common"
	sbkube "github.com/spdk/spdk-csi/pkg/kubernetes"
	"github.com/spdk/spdk-csi/pkg/util"
)

type nodeServer struct {
	*csicommon.DefaultNodeServer
	mounter     mount.Interface
	volumeLocks *util.VolumeLocks
	kubeClient  kubernetes.Interface
	manager     *sbkube.Manager
	guardian    *util.Guardian
}

//nolint:unparam // error return kept for constructor symmetry / future use
func newNodeServer(d *csicommon.CSIDriver, kubeClient kubernetes.Interface) (*nodeServer, error) {
	ns := &nodeServer{
		DefaultNodeServer: csicommon.NewDefaultNodeServer(d),
		mounter:           mount.New(""),
		volumeLocks:       util.NewVolumeLocks(),
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
	gcfg := util.NewDefaultGuardianConfig(nodeName)
	guardian, gerr := util.StartGuardian(context.Background(), gcfg, manager)
	if gerr != nil {
		klog.Errorf("failed to start guardian: %v", gerr)
	} else {
		ns.guardian = guardian
	}

	go util.MonitorConnection(func(lvolID string) {
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

	sizeBytes, err := getBlockSizeBytes(volumePath)
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
	srcClient util.ClusterAPI,
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
	tgtClient, err := util.NewsimplyBlockClient(ctx, targetClusterID, targetPoolID)
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

	isStaged, err := ns.isStaged(stagingTargetPath)
	if err != nil {
		klog.Errorf("failed to check isStaged, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if isStaged {
		// A staged volume whose backing NVMe-oF device was lost leaves a dead
		// (EIO) mount that isStaged still reports as staged. Repair it in place
		// instead of short-circuiting.
		if !ns.stagingMountDead(stagingTargetPath) {
			klog.Warning("volume already staged")
			return &csi.NodeStageVolumeResponse{}, nil
		}
		klog.Warningf("volume %s already staged but its mount is dead; restaging", volumeID)
		if err := ns.restageVolume(ctx, volumeID, stagingTargetPath, stagingParentPath, req.GetVolumeCapability()); err != nil { //nolint:lll // unwrappable string/log/signature
			return nil, status.Errorf(codes.Internal, "restage volume %s: %v", volumeID, err)
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}

	var initiator util.SpdkCsiInitiator
	vc := req.GetVolumeContext()

	vc["stagingParentPath"] = stagingParentPath

	if ns.kubeClient != nil {
		nodeName := ns.Driver.GetNodeID()
		node, nodeErr := ns.kubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if nodeErr == nil {
			vc["hostNQN"] = fmt.Sprintf("nqn.2014-08.io.simplyblock:uuid:%s", node.UID)
		} else {
			klog.Warningf("failed to get node %s for hostNQN: %v", nodeName, nodeErr)
		}
	}

	if spdkVol, err := parseVolumeID(volumeID); err == nil {
		vc["poolID"] = spdkVol.poolID

		// Re-fetch connection info from the backend when:
		// - the volume was provisioned against a pool with allowed_hosts (nqn/targetType empty), or
		// - the volume may have been failed over (always refresh so the backend can redirect
		//   to the clone and return target_lvol_id for correct device lookup).
		if sbcClient, clientErr := util.NewsimplyBlockClient(ctx, spdkVol.clusterID, spdkVol.poolID); clientErr == nil {
			connInfo, infoErr := sbcClient.VolumeInfo(ctx, spdkVol.lvolID, vc["hostNQN"])
			if infoErr != nil {
				if errors.Is(infoErr, util.ErrVolumeNotFound) {
					// Source volume was deleted (migration with --delete-source).
					// Query the replication relationship to find the active volume
					// on the target cluster and redirect to it.
					connInfo = ns.redirectToActiveVolume(ctx, sbcClient, spdkVol.lvolID, volumeID, vc)
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

	initiator, err = util.NewSpdkCsiInitiator(vc)
	if err != nil {
		klog.Errorf("failed to create spdk initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	devicePath, err := initiator.Connect(ctx) // idempotent
	if err != nil {
		klog.Errorf("failed to connect initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer func() {
		if err != nil {
			initiator.Disconnect(ctx) //nolint:errcheck // ignore error
		}
	}()
	if err = ns.stageVolume(ctx, devicePath, stagingTargetPath, req, vc); err != nil { // idempotent
		klog.Errorf("failed to stage volume, volumeID: %s devicePath:%s err: %v", volumeID, devicePath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	vc["devicePath"] = devicePath
	// stash VolumeContext to stagingParentPath (useful during Unstage as it has no
	// VolumeContext passed to the RPC as per the CSI spec)
	err = util.StashVolumeContext(req.GetVolumeContext(), stagingParentPath)
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

	err := ns.deleteMountPoint(stagingTargetPath) // idempotent
	if err != nil {
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Errorf(codes.Internal, "unstage volume %s failed: %s", volumeID, err)
	}

	volumeContext, err := util.LookupVolumeContext(stagingParentPath)
	if err != nil {
		klog.Errorf("failed to lookup volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	initiator, err := util.NewSpdkCsiInitiator(volumeContext)
	if err != nil {
		klog.Errorf("failed to create spdk initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cleanupCancel()
	err = initiator.Disconnect(cleanupCtx) // idempotent
	if err != nil {
		klog.Errorf("failed to disconnect initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := util.CleanUpVolumeContext(stagingParentPath); err != nil {
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

	err := ns.deleteMountPoint(req.GetTargetPath()) // idempotent
	if err != nil {
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", req.GetTargetPath(), err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	if ns.guardian != nil {
		ns.guardian.RegisterUnpublishByTargetPath(req.TargetPath)
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
	volumeContext, err := util.LookupVolumeContext(stagingParentPath)
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

	resizer := mount.NewResizeFs(exec.New())
	needsResize, err := resizer.NeedResize(devicePath, volumeMountPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if volume %s needs resizing: %v", volumeID, err)
	}

	if needsResize {
		resized, err := resizer.Resize(devicePath, volumeMountPath)
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

// defaultXFSStripeUnit and defaultXFSStripeWidth are the fallback mkfs.xfs
// stripe geometry used when the StorageClass does not override xfs_su/xfs_sw.
// These are a starting point based on initial testing, not a computed value
// derived from cluster NDCS (which did not reliably improve performance).
const (
	defaultXFSStripeUnit  = "16k"
	defaultXFSStripeWidth = "1"
)

// xfsStripeOptions returns mkfs.xfs format options that set the stripe geometry
// from the StorageClass-provided xfs_su/xfs_sw parameters, falling back to
// defaultXFSStripeUnit/defaultXFSStripeWidth when unset. Both parameters must
// be set together; if only one is set, the defaults are used instead.
func xfsStripeOptions(volumeContext map[string]string) []string {
	su := volumeContext["xfs_su"]
	sw := volumeContext["xfs_sw"]
	switch {
	case su == "" && sw == "":
		su, sw = defaultXFSStripeUnit, defaultXFSStripeWidth
	case su == "" || sw == "":
		klog.Warningf(
			"xfsStripeOptions: xfs_su and xfs_sw must both be set; got xfs_su=%q xfs_sw=%q, falling back to defaults su=%s,sw=%s", //nolint:lll // unwrappable string/log/signature
			su,
			sw,
			defaultXFSStripeUnit,
			defaultXFSStripeWidth,
		)
		su, sw = defaultXFSStripeUnit, defaultXFSStripeWidth
	}
	if swVal, err := strconv.Atoi(sw); err != nil || swVal <= 0 {
		klog.Warningf("xfsStripeOptions: xfs_sw must be a positive integer, got %q, skipping stripe alignment", sw)
		return nil
	}
	return []string{"-d", fmt.Sprintf("su=%s,sw=%s", su, sw), "-l", fmt.Sprintf("su=%s", su)}
}

// xfsFormatConfigPath is the mkfs.xfs config file that pins which on-disk features
// new XFS volumes are created with. xfsprogs ships it; the container image only has
// to contain a matching xfsprogs. Kept in sync with the assertion in
// deploy/image/Dockerfile_base.
const xfsFormatConfigPath = "/usr/share/xfsprogs/mkfs/lts_5.15.conf"

// xfsFeatureOptions returns the mkfs.xfs option that pins the on-disk feature set to
// what the oldest supported host kernel can mount.
//
// mkfs.xfs derives feature bits from its own defaults and never asks the running
// kernel what it supports, so a newer xfsprogs happily writes a filesystem that the
// host then refuses to mount. The el10 base defaults to parent=1 (parent pointers),
// which implies EXCHRANGE and yields sb_features_incompat=0xeb, while RHEL/Rocky 9
// kernels accept only 0xb:
//
//	XFS (nvme0n1): Superblock has unknown incompatible features (0xc0) enabled.
//	XFS (nvme0n1): Filesystem cannot be safely mounted by this kernel.
//	XFS (nvme0n1): SB validate failed with error -22.
//
// Unlike the ext4 equivalent there is no repair path: XFS features can only be added,
// never removed, and such a filesystem cannot be mounted even read-only. Prevention
// is the only option.
//
// lts_5.15.conf is chosen over the closer lts_6.x baselines because it sits below the
// floor of every el9 minor instead of tracking vendor backports -- 9.5 accepts 0xb
// while 9.8 additionally accepts EXCHRANGE and NREXT64. The options compose with
// xfsStripeOptions: stripe geometry lives in sb_unit/sb_width/sb_logsunit and is
// independent of the feature words.
//
// An unusable config file is a warning rather than an error: mkfs.xfs treats an
// unreadable -c options= path as fatal, so failing open keeps an image that predates
// this pin able to format volumes at all, which is the safer failure for the el9
// image whose built-in defaults already produce 0xb.
func xfsFeatureOptions() []string {
	if err := checkXFSFormatConfig(); err != nil {
		klog.Warningf(
			"xfsFeatureOptions: %s unusable (%v); formatting with mkfs.xfs built-in defaults, which on a newer xfsprogs may produce a filesystem this kernel cannot mount", //nolint:lll // unwrappable string/log/signature
			xfsFormatConfigPath,
			err,
		)
		return nil
	}
	return []string{"-c", "options=" + xfsFormatConfigPath}
}

// checkXFSFormatConfig reports whether mkfs.xfs will actually be able to consume the
// pinned config.
func checkXFSFormatConfig() error {
	f, err := os.Open(xfsFormatConfigPath)
	if err != nil {
		return err
	}
	defer deferrers.Close(f)

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file (mode %s)", info.Mode())
	}
	return nil
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

	mounted, err := ns.createMountPoint(stagingPath)
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
	fs, probeErr := getDiskFormat(devicePath)
	fs, err = classifyDiskFormat(devicePath, fs, probeErr)
	if err != nil {
		return err
	}

	// blkid reporting nothing means either that the device carries no filesystem
	// or that it could not be read, and the two are indistinguishable from its
	// exit code alone. A claim that records a filesystem settles it: the volume
	// was formatted once, so this reading is a failed probe rather than a blank
	// device, and it is mounted as what the claim says is down there.
	//
	// It is mounted as the recorded filesystem, not the requested one, because
	// those disagree exactly when it matters — a volume formatted before its
	// StorageClass changed — and mounting ext4 as XFS fails. The mount flags
	// follow the same filesystem for the same reason: XFS needs nouuid, and
	// deriving the flags from the request would drop it.
	//
	// A mount that fails here is the correct outcome and must stay one. It means
	// the claim's record disagrees with the device, or the device is genuinely
	// dead, and neither is a reason to format: falling back to mkfs would
	// reinstate the data loss this branch exists to prevent.
	mntFlags := stagingMountFlags(fsType, req.GetVolumeCapability())
	mounter := mount.SafeFormatAndMount{Interface: ns.mounter, Exec: exec.New()}
	if fs == "" {
		annotated, err := ns.annotatedFilesystem(ctx, req.GetVolumeId(), volumeContext)
		if err != nil {
			return err
		}
		if annotated != "" {
			if annotated != fsType {
				klog.Warningf(
					"volume %s: claim records a %s filesystem but the volume asks for %s; mounting %s at %s as %s without reformatting", //nolint:lll // unwrappable string/log/signature
					req.GetVolumeId(), annotated, fsType, devicePath, stagingPath, annotated,
				)
			}
			volumeContext[stagedFsTypeKey] = annotated
			return mounter.Mount(
				devicePath,
				stagingPath,
				annotated,
				stagingMountFlags(annotated, req.GetVolumeCapability()),
			)
		}
	}

	// Record what was actually staged: a later restage remounts an existing
	// filesystem, and the volume capability alone no longer answers which one it
	// is once the annotation has overridden it.
	volumeContext[stagedFsTypeKey] = fsType

	formatOptions := []string{}
	if fsType == "xfs" {
		formatOptions = append(formatOptions, xfsFeatureOptions()...)
		formatOptions = append(formatOptions, xfsStripeOptions(volumeContext)...)
	}

	klog.Infof("mount %s to %s, fstype: %s, flags: %v", devicePath, stagingPath, fsType, mntFlags)
	klog.Infof("formatOptions %v", formatOptions)
	err = mounter.FormatAndMountSensitiveWithFormatOptions(
		devicePath,
		stagingPath,
		fsType,
		mntFlags,
		nil,
		formatOptions,
	)
	if err != nil {
		return err
	}

	if fsType == "ext4" {
		reserved := volumeContext["tune2fs_reserved_blocks"]
		if reserved != "" {
			cmd := osexec.Command("tune2fs", "-m", reserved, devicePath)
			output, err := cmd.CombinedOutput()
			if err != nil {
				klog.Errorf(
					"Failed to apply tune2fs -m %s on %s: %v\nOutput: %s",
					reserved,
					devicePath,
					err,
					string(output),
				)
				return fmt.Errorf("tune2fs failed: %w", err)
			}
			klog.Infof("Applied tune2fs -m %s on %s", reserved, devicePath)
		} else {
			klog.Infof("No tune2fs_reserved_blocks set; skipping tune2fs adjustment")
		}
	}

	// The device now definitely carries fsType: either mkfs just put it there, or
	// the probe found it there and the mount above would have failed on any other
	// filesystem. Record that on the claim, so what a volume is formatted with is
	// answerable without a node to run blkid on.
	ns.recordOnDiskFilesystem(ctx, req.GetVolumeId(), volumeContext, fsType)

	return nil
}

// unknownDiskFormat is what getDiskFormat reports for a device that carries a
// partition table rather than a filesystem: something is on it, but not
// something that can be mounted, and formatting it would destroy whatever it is.
const unknownDiskFormat = "unknown data, probably partitions"

// classifyDiskFormat turns a blkid probe of devicePath into the one thing
// staging needs to know: the filesystem already on the device, empty when the
// device is positively blank.
//
// It errors on every reading that is neither of those. Formatting is
// irreversible, and both remaining readings — a probe that failed, and a
// partition table where a filesystem was expected — leave it open whether the
// device holds somebody's data. Staging fails there instead of formatting
// through the doubt; the next attempt probes the device again.
func classifyDiskFormat(devicePath, fs string, probeErr error) (string, error) {
	if probeErr != nil {
		return "", fmt.Errorf(
			"cannot read the on-disk filesystem of %s, refusing to stage a device whose contents are unknown: %w",
			devicePath, probeErr,
		)
	}
	if fs == unknownDiskFormat {
		return "", fmt.Errorf(
			"device %s carries a partition table rather than a filesystem, refusing to stage it",
			devicePath,
		)
	}
	return fs, nil
}

func getDiskFormat(disk string) (string, error) {
	args := []string{"-p", "-s", "TYPE", "-s", "PTTYPE", "-o", "export", disk}
	klog.V(4).Infof("Attempting to determine if disk %q is formatted using blkid with args: (%v)", disk, args)
	dataOut, err := osexec.Command("blkid", args...).CombinedOutput()
	output := string(dataOut)
	klog.V(4).Infof("Output: %q", output)

	if err != nil {
		var exit *osexec.ExitError
		if errors.As(err, &exit) {
			if exit.ExitCode() == 2 {
				// Disk device is unformatted.
				// For `blkid`, if the specified token (TYPE/PTTYPE, etc) was
				// not found, or no (specified) devices could be identified, an
				// exit code of 2 is returned.
				return "", nil
			}
		}
		klog.Errorf("Could not determine if disk %q is formatted (%v)", disk, err)
		return "", err
	}

	var fstype, pttype string

	lines := strings.Split(output, "\n")
	for _, l := range lines {
		if len(l) <= 0 {
			// Ignore empty line.
			continue
		}
		cs := strings.Split(l, "=")
		if len(cs) != 2 {
			return "", fmt.Errorf("blkid returns invalid output: %s", output)
		}
		// TYPE is filesystem type, and PTTYPE is partition table type, according
		// to https://www.kernel.org/pub/linux/utils/util-linux/v2.21/libblkid-docs/.
		switch cs[0] {
		case "TYPE":
			fstype = cs[1]
		case "PTTYPE":
			pttype = cs[1]
		}
	}

	if len(pttype) > 0 {
		klog.V(4).Infof("Disk %s detected partition table type: %s", disk, pttype)
		// Returns a special non-empty string as filesystem type, then kubelet
		// will not format it.
		return unknownDiskFormat, nil
	}

	return fstype, nil
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

// supportedOnDiskFilesystems are the filesystems this driver formats and mounts.
// The annotation is writable by anyone who can edit the claim, so a value
// outside this set is ignored rather than passed on to mkfs.
var supportedOnDiskFilesystems = map[string]bool{
	"ext4": true,
	"xfs":  true,
}

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
		spdkVol, err := parseVolumeID(volumeID)
		if err != nil {
			return nil, fmt.Errorf("resolve claim for volume %s: %w", volumeID, err)
		}

		pv, err := ns.manager.PersistentVolumeByLogicalVolumeID(ctx, spdkVol.lvolID)
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
	if !supportedOnDiskFilesystems[fsType] {
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

	if fsType == "xfs" {
		// XFS refuses to mount two filesystems with the same UUID; nouuid lets a
		// volume and its clone/restored snapshot mount on the same node.
		flags = append(flags, "nouuid")
	}

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

// stagingMountDead reports whether stagingPath is a dead/corrupted mount — the
// state left behind when total NVMe-oF path loss makes the kernel remove the
// backing device. Such a mount returns ENOTCONN/ESTALE/EIO on access, which
// mount.IsCorruptedMnt detects.
func (ns *nodeServer) stagingMountDead(stagingPath string) bool {
	if _, err := ns.mounter.IsMountPoint(stagingPath); err != nil {
		return mount.IsCorruptedMnt(err)
	}
	// IsMountPoint can still succeed on a mount whose device just vanished;
	// a stat of the path then fails with an EIO-class error.
	fi, err := os.Stat(stagingPath)
	if err != nil {
		return mount.IsCorruptedMnt(err)
	}
	// Some filesystems (notably ext4) do NOT shut down when their backing block
	// device is removed on total NVMe-oF path loss — unlike XFS, which goes EIO
	// and is caught above. IsMountPoint and stat then both succeed from cache, so
	// the dead mount looks healthy and never gets restaged. Detect it by checking
	// that the block device backing the mount still exists: the mountpoint's
	// st_dev gives the device major:minor, and once the kernel removes the device
	// /sys/dev/block/<major>:<minor> disappears. A later reconnect gets a NEW
	// major:minor, but this mount stays bound to the old (gone) one until it is
	// restaged, so this never false-positives on a healthy, read-only, or full fs.
	return backingBlockDeviceGone(fi)
}

// backingBlockDeviceGone reports whether the block device that backs the mounted
// filesystem described by fi no longer exists in sysfs. It returns false for
// filesystems with an anonymous super-block device (tmpfs/overlay/etc.), which
// have no /sys/dev/block entry to check.
func backingBlockDeviceGone(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	dev := uint64(st.Dev) //nolint:unconvert // st.Dev is uint64 on linux/amd64, int32 elsewhere
	if unix.Major(dev) == 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/sys/dev/block/%d:%d", unix.Major(dev), unix.Minor(dev)))
	return os.IsNotExist(err)
}

// forceUnmountStaging detaches a dead staging mount. A lazy unmount (umount -l)
// is used because a normal unmount can hang or fail when the backing device is
// gone. The staging directory itself is preserved for the remount.
func (ns *nodeServer) forceUnmountStaging(stagingPath string) error {
	out, err := osexec.Command("umount", "-l", stagingPath).CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out))
		if strings.Contains(msg, "not mounted") || strings.Contains(msg, "not found") {
			return nil
		}
		return fmt.Errorf("lazy unmount %s: %w (%s)", stagingPath, err, strings.TrimSpace(string(out)))
	}
	return nil
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
		if ns.stagingMountDead(stagingTargetPath) {
			return ns.restageVolume(ctx, req.GetVolumeId(), stagingTargetPath, stagingParentPath, volCap)
		}
	}
	return nil
}

// ensureDeviceConnected reconnects a block volume's NVMe-oF device if it has
// gone away. The by-id device path is stable across reconnects, so only the
// connection needs re-establishing (no mount). Idempotent.
func (ns *nodeServer) ensureDeviceConnected(ctx context.Context, volumeID, stagingParentPath string) error {
	volumeContext, err := util.LookupVolumeContext(stagingParentPath)
	if err != nil {
		return fmt.Errorf("lookup volume context: %w", err)
	}
	if devicePath := volumeContext["devicePath"]; devicePath != "" && deviceExists(devicePath) {
		return nil
	}

	klog.Warningf("block volume %s device is gone; reconnecting NVMe-oF", volumeID)
	initiator, err := util.NewSpdkCsiInitiator(volumeContext)
	if err != nil {
		return fmt.Errorf("new initiator: %w", err)
	}
	devicePath, err := initiator.Connect(ctx) // idempotent
	if err != nil {
		return fmt.Errorf("reconnect device: %w", err)
	}
	if volumeContext["devicePath"] != devicePath {
		volumeContext["devicePath"] = devicePath
		if err := util.StashVolumeContext(volumeContext, stagingParentPath); err != nil {
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

	volumeContext, err := util.LookupVolumeContext(stagingParentPath)
	if err != nil {
		return fmt.Errorf("lookup volume context: %w", err)
	}

	if err := ns.forceUnmountStaging(stagingTargetPath); err != nil {
		return fmt.Errorf("unmount dead staging mount: %w", err)
	}

	initiator, err := util.NewSpdkCsiInitiator(volumeContext)
	if err != nil {
		return fmt.Errorf("new initiator: %w", err)
	}
	devicePath, err := initiator.Connect(ctx) // idempotent: re-establishes the lost device
	if err != nil {
		return fmt.Errorf("reconnect device: %w", err)
	}

	if _, err := ns.createMountPoint(stagingTargetPath); err != nil {
		return fmt.Errorf("recreate staging dir: %w", err)
	}
	// Plain Mount, not FormatAndMount: the volume already holds a filesystem and
	// reformatting would destroy data.
	fsType := stagedFsType(volumeContext, volCap)
	if err := ns.mounter.Mount(devicePath, stagingTargetPath, fsType, stagingMountFlags(fsType, volCap)); err != nil {
		return fmt.Errorf("remount device %s at %s: %w", devicePath, stagingTargetPath, err)
	}

	volumeContext["devicePath"] = devicePath
	if err := util.StashVolumeContext(volumeContext, stagingParentPath); err != nil {
		klog.Warningf("restageVolume: failed to re-stash volume context for %s: %v", volumeID, err)
	}
	klog.Infof("restaged volume %s on fresh device %s", volumeID, devicePath)
	return nil
}

// isStaged if stagingPath is a mount point, it means it is already staged, and vice versa
func (ns *nodeServer) isStaged(stagingPath string) (bool, error) {
	isMount, err := ns.mounter.IsMountPoint(stagingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		} else if mount.IsCorruptedMnt(err) {
			return true, nil
		}
		klog.Warningf("check is stage error: %v", err)
		return false, err
	}
	return isMount, nil
}

// must be idempotent
func (ns *nodeServer) publishVolume(stagingPath string, req *csi.NodePublishVolumeRequest) error {
	targetPath := req.GetTargetPath()

	fsType := req.GetVolumeCapability().GetMount().GetFsType()

	if req.GetVolumeCapability().GetBlock() != nil {
		stagingParentPath := req.GetStagingTargetPath()
		volumeContext, err := util.LookupVolumeContext(stagingParentPath)
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

		if err := ns.ensureCleanTargetPath(targetPath); err != nil {
			return status.Errorf(codes.Internal, "Could not cleanup mount target %q: %v", targetPath, err)
		}

		if err = ns.MakeFile(targetPath); err != nil {
			if removeErr := os.Remove(targetPath); removeErr != nil {
				return status.Errorf(codes.Internal, "Could not remove mount target %q: %v", targetPath, removeErr)
			}
			return status.Errorf(codes.Internal, "Could not create file %q: %v", targetPath, err)
		}
	} else if req.GetVolumeCapability().GetMount() != nil {
		mounted, err := ns.createMountPoint(targetPath)
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

// create mount point if not exists, return whether already mounted
func (ns *nodeServer) createMountPoint(path string) (bool, error) {
	isMount, err := ns.mounter.IsMountPoint(path)
	if os.IsNotExist(err) {
		isMount = false
		err = os.MkdirAll(path, 0o755)
	}
	if isMount {
		klog.Infof("%s already mounted", path)
	}
	return isMount, err
}

// unmount and delete mount point, must be idempotent
func (ns *nodeServer) deleteMountPoint(path string) error {
	isMount, err := ns.mounter.IsMountPoint(path)
	if err != nil {
		if os.IsNotExist(err) {
			klog.Infof("%s already deleted", path)
			return nil
		} else if mount.IsCorruptedMnt(err) {
			klog.Warningf("Corrupted mount point detected at %s", path)
			isMount = true
		} else {
			klog.Errorf("Error checking mount point %s: %v", path, err)
			return err
		}
	}

	if isMount {
		err = ns.mounter.Unmount(path)
		if err != nil {
			return err
		}
	}
	return os.RemoveAll(path)
}

func (ns *nodeServer) MakeFile(path string) error {
	// Create file
	newFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0750)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", path, err)
	}
	if err := newFile.Close(); err != nil {
		return fmt.Errorf("failed to close file %s: %w", path, err)
	}
	return nil
}

// ensureCleanTargetPath makes sure targetPath is not a mountpoint and is removed.
// idempotent
func (ns *nodeServer) ensureCleanTargetPath(targetPath string) error {
	isMount, err := ns.mounter.IsMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if mount.IsCorruptedMnt(err) {
			isMount = true
		} else {
			return err
		}
	}

	if isMount {
		if err := ns.mounter.Unmount(targetPath); err != nil {
			_ = osexec.Command("umount", "-l", targetPath).Run()
		}
	}

	if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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

func getBlockSizeBytes(volumePath string) (uint64, error) {
	if size, err := ioctlBlkGetSize64(volumePath); err == nil && size > 0 {
		return size, nil
	}

	rp, err := filepath.EvalSymlinks(volumePath)
	if err == nil && rp != "" && rp != volumePath {
		if size, err2 := ioctlBlkGetSize64(rp); err2 == nil && size > 0 {
			return size, nil
		}
	}

	return 0, fmt.Errorf("BLKGETSIZE64 ioctl failed for %q", volumePath)
}

func ioctlBlkGetSize64(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer deferrers.Close(f)

	// blkGetSize64 is the Linux BLKGETSIZE64 ioctl code.
	// It returns the total size (in bytes) of a block device.
	var blkGetSize64 = 0x80081272

	var size uint64
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL, //nolint:staticcheck // SA1019: Linux target; direct ioctl syscall is intended
		f.Fd(),
		uintptr(blkGetSize64),
		uintptr(unsafe.Pointer(&size)),
	)
	if errno != 0 {
		return 0, errno
	}
	return size, nil
}
