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
	"github.com/simplyblock/atlas/kube"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	mount "k8s.io/mount-utils"
	"k8s.io/utils/exec"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	csicommon "github.com/spdk/spdk-csi/pkg/csi-common"
	sbkube "github.com/spdk/spdk-csi/pkg/kubernetes"
	"github.com/spdk/spdk-csi/pkg/util"
)

// vdoCapableMarkerPath is where the DaemonSet's postStart hook records the outcome of
// installing/loading kvdo (see helm-charts .../templates/node.yaml) -- read at node-server
// startup by advertiseVDOCapability.
const vdoCapableMarkerPath = "/var/run/simplyblock/vdo-capable/marker"

// vdoCapableLabelValue is the value of vdoCapableLabelKey (and the marker file's own
// contents) when a node is VDO-capable.
const vdoCapableLabelValue = "true"

// vdoParams parses the two client-side VDO parameters out of a volume context, tolerating
// the same boolean string forms kube.BoolParam already accepts for encryption/replicate
// elsewhere in this driver -- a plain == "true" comparison would silently never match the
// "True" that mergeStorageClassParameters' boolStr actually emits. wantsVDO is true if
// either is set: a dedup-only volume still needs a working kvdo module, not
// client_compression alone.
func vdoParams(vc map[string]string) (compression, deduplication, wantsVDO bool) {
	compression, _ = kube.BoolParam(vc, paramClientCompression, false)
	deduplication, _ = kube.BoolParam(vc, paramClientDeduplication, false)
	return compression, deduplication, compression || deduplication
}

type nodeServer struct {
	*csicommon.DefaultNodeServer
	mounter     mount.Interface
	volumeLocks *util.VolumeLocks
	kubeClient  kubernetes.Interface
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

	if ns.kubeClient != nil {
		go ns.advertiseVDOCapability(context.Background())
	}

	return ns, nil
}

// advertiseVDOCapability reads the VDO-capability marker file written by this DaemonSet's
// postStart hook (kmod-kvdo/vdo install + modprobe, see helm-charts node.yaml) and patches
// this Node's simplyblock.io/vdo-capable label to match. Runs in the background and retries
// with a bounded timeout, since the postStart hook (a dnf install on first run) can take
// longer than node-server startup should block on -- without the retry, a fast-starting
// node server could read "marker not found yet" and wrongly label the node incapable.
func (ns *nodeServer) advertiseVDOCapability(ctx context.Context) {
	const (
		maxWait  = 5 * time.Minute
		interval = 5 * time.Second
	)

	nodeName := ns.Driver.GetNodeID()
	if nodeName == "" {
		klog.Warning("advertiseVDOCapability: no node ID, skipping")
		return
	}

	deadline := time.Now().Add(maxWait)
	var capable bool
	for {
		data, err := os.ReadFile(vdoCapableMarkerPath)
		if err == nil {
			capable = strings.TrimSpace(string(data)) == vdoCapableLabelValue
			break
		}
		if time.Now().After(deadline) {
			klog.Warningf(
				"advertiseVDOCapability: marker file %s never appeared after %s, treating node as not VDO-capable: %v", //nolint:lll
				vdoCapableMarkerPath, maxWait, err,
			)
			capable = false
			break
		}
		time.Sleep(interval)
	}

	if err := ns.patchVDOCapableLabel(ctx, nodeName, capable); err != nil {
		klog.Errorf("advertiseVDOCapability: failed to patch node %s label: %v", nodeName, err)
	}
}

func (ns *nodeServer) patchVDOCapableLabel(ctx context.Context, nodeName string, capable bool) error {
	patch := struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}{}
	patch.Metadata.Labels = map[string]string{vdoCapableLabelKey: strconv.FormatBool(capable)}

	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal label patch: %w", err)
	}
	_, err = ns.kubeClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, body, metav1.PatchOptions{})
	return err
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

	// Always present, value only ever changes -- CSINode's topology KEY SET is captured
	// once at plugin registration (moments after this pod starts), while
	// advertiseVDOCapability patches the underlying label asynchronously in the
	// background (it can take minutes, waiting on the postStart hook's dnf install).
	// Conditionally omitting this key when false would mean it's essentially never
	// present at registration time, permanently breaking the topology gate for that
	// node's CSINode entry -- external-provisioner reads the VALUE fresh from the live
	// Node object on every provision, so only the key's presence needs to be stable,
	// exactly like topologyKeyStorageNodeUUIDPrefix above.
	vdoCapable := node.Labels[vdoCapableLabelKey]
	if vdoCapable == "" {
		vdoCapable = "false"
	}
	segments[vdoCapableLabelKey] = vdoCapable

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

		// When the volume was provisioned against a pool with allowed_hosts, the controller
		// couldn't fetch connection info (no host NQN available there). Re-fetch it here using
		// the node's host NQN so that NewSpdkCsiInitiator gets the fields it needs.
		if vc["nqn"] == "" || vc["targetType"] == "" {
			if sbcClient, clientErr := util.NewsimplyBlockClient(ctx, spdkVol.clusterID, spdkVol.poolID); clientErr == nil {
				connInfo, infoErr := sbcClient.VolumeInfo(ctx, spdkVol.lvolID, vc["hostNQN"])
				if infoErr != nil {
					klog.Errorf("failed to fetch volume connection info for %s: %v", volumeID, infoErr)
				} else {
					for k, v := range connInfo {
						if vc[k] == "" {
							vc[k] = v
						}
					}
				}
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

	// A VDO device sits between the raw NVMe-oF device and the filesystem whenever either
	// client-side parameter is set -- dedup-only volumes still need a working kvdo module,
	// so this is not gated on client_compression alone.
	mountDevicePath := devicePath
	if compression, deduplication, wantsVDO := vdoParams(vc); wantsVDO {
		lvolID := volumeID
		if spdkVol, perr := parseVolumeID(volumeID); perr == nil {
			lvolID = spdkVol.lvolID
		}
		// Always resolve a possible byte-level clone identity collision before touching
		// the device -- driven by the device's actual on-disk VG name, not by whether the
		// controller told us this was a clone, so it's correct whether or not that fact
		// was threaded through the volume context.
		if err = util.ResolveClonedVDO(ctx, devicePath, lvolID); err != nil {
			klog.Errorf("failed to resolve cloned VDO identity, volumeID: %s err: %v", volumeID, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
		mountDevicePath, err = util.CreateOrAttachVDO(ctx, devicePath, lvolID, compression, deduplication)
		if err != nil {
			klog.Errorf("failed to create/attach VDO, volumeID: %s err: %v", volumeID, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if err = ns.stageVolume(mountDevicePath, stagingTargetPath, req, vc); err != nil { // idempotent
		klog.Errorf("failed to stage volume, volumeID: %s devicePath:%s err: %v", volumeID, mountDevicePath, err)
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

	// VDO must come down before the raw device it sits on is disconnected -- disconnecting
	// first would leave a live dm-vdo stack mapped on top of a device that just vanished,
	// the same orphaned-stack state documented in the design doc's spike log. Deactivate
	// only -- NodeUnstageVolume fires any time no pod on this node needs the volume mounted
	// (including a routine pod delete+recreate on the same node), not only when the volume
	// is being deleted; CreateOrAttachVDO's vgchange -ay reactivates this same state later.
	if _, _, wantsVDO := vdoParams(volumeContext); wantsVDO {
		lvolID := volumeID
		if spdkVol, perr := parseVolumeID(volumeID); perr == nil {
			lvolID = spdkVol.lvolID
		}
		if err = util.DeactivateVDO(cleanupCtx, lvolID); err != nil {
			klog.Errorf("failed to deactivate VDO device, volumeID: %s err: %v", volumeID, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

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
	// on this node (e.g. a same-node pod replacement), so NodePublish is the
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
		if spdkVol, err := parseVolumeID(volumeID); err == nil {
			ns.guardian.RegisterUnpublish(spdkVol.lvolID, req.TargetPath)
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

	resizeDevicePath := devicePath
	if _, _, wantsVDO := vdoParams(volumeContext); wantsVDO {
		lvolID := volumeID
		if spdkVol, perr := parseVolumeID(volumeID); perr == nil {
			lvolID = spdkVol.lvolID
		}
		grownPath, growErr := util.GrowVDO(ctx, devicePath, lvolID)
		if growErr != nil {
			return nil, status.Errorf(codes.Internal, "failed to grow VDO device for volume %s: %v", volumeID, growErr)
		}
		resizeDevicePath = grownPath
	}

	resizer := mount.NewResizeFs(exec.New())
	needsResize, err := resizer.NeedResize(resizeDevicePath, volumeMountPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if volume %s needs resizing: %v", volumeID, err)
	}

	if needsResize {
		resized, err := resizer.Resize(resizeDevicePath, volumeMountPath)
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
// new xfs volumes are created with. xfsprogs ships it; the container image only has
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
// Unlike the ext4 equivalent there is no repair path: xfs features can only be added,
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
	mntFlags := stagingMountFlags(req.GetVolumeCapability())
	formatOptions := []string{}

	if fsType == "xfs" {
		formatOptions = append(formatOptions, xfsFeatureOptions()...)
		// VDO virtualizes and relocates blocks, so once it's in play the filesystem is no
		// longer directly on the erasure-coded backend device these stripe hints were
		// computed for -- applying them to a VDO virtual device is actively misleading, not
		// just useless. xfsFeatureOptions is unaffected: on-disk feature-bit compatibility
		// across kernel versions has nothing to do with the backend's striping geometry.
		_, _, usesVDO := vdoParams(volumeContext)
		if !usesVDO {
			formatOptions = append(formatOptions, xfsStripeOptions(volumeContext)...)
		}
	}

	klog.Infof("mount %s to %s, fstype: %s, flags: %v", devicePath, stagingPath, fsType, mntFlags)
	klog.Infof("formatOptions %v", formatOptions)
	mounter := mount.SafeFormatAndMount{Interface: ns.mounter, Exec: exec.New()}
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

	return nil
}

// fsTypeOrDefault returns the requested filesystem type, defaulting to ext4.
func fsTypeOrDefault(volCap *csi.VolumeCapability) string {
	if fsType := volCap.GetMount().GetFsType(); fsType != "" {
		return fsType
	}
	return "ext4"
}

// stagingMountFlags builds the mount flags used when mounting a volume at its
// staging path, so the initial stage and a later restage stay consistent.
func stagingMountFlags(volCap *csi.VolumeCapability) []string {
	flags := append([]string{}, volCap.GetMount().GetMountFlags()...)

	if volCap.GetMount().GetFsType() == "xfs" {
		// xfs refuses to mount two filesystems with the same uuid; nouuid lets a
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
	// device is removed on total NVMe-oF path loss — unlike xfs, which goes EIO
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

	mountDevicePath := devicePath
	if compression, deduplication, wantsVDO := vdoParams(volumeContext); wantsVDO {
		lvolID := volumeID
		if spdkVol, perr := parseVolumeID(volumeID); perr == nil {
			lvolID = spdkVol.lvolID
		}
		// Reattach only -- CreateOrAttachVDO is idempotent and reactivates an existing VG
		// rather than recreating it, matching this function's own "never reformat, data
		// already exists" invariant.
		mountDevicePath, err = util.CreateOrAttachVDO(ctx, devicePath, lvolID, compression, deduplication)
		if err != nil {
			return fmt.Errorf("reattach VDO device for %s: %w", volumeID, err)
		}
	}

	if _, err := ns.createMountPoint(stagingTargetPath); err != nil {
		return fmt.Errorf("recreate staging dir: %w", err)
	}
	// Plain Mount, not FormatAndMount: the volume already holds a filesystem and
	// reformatting would destroy data.
	if err := ns.mounter.Mount(mountDevicePath, stagingTargetPath, fsTypeOrDefault(volCap), stagingMountFlags(volCap)); err != nil { //nolint:lll // unwrappable string/log/signature
		return fmt.Errorf("remount device %s at %s: %w", mountDevicePath, stagingTargetPath, err)
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
