// Staging: attaching the volume's device to this node and mounting it at the
// staging path, and the format decision that sits between the two.
package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/simplyblock/atlas/nqn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/csi-driver/internal/clusters"
	"github.com/simplyblock/csi-driver/internal/controlplane"
	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/initiator"
	"github.com/simplyblock/csi-driver/internal/mount"
)

func (ns *Server) NodeStageVolume(
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

	if spdkVol, err := csicommon.ParseVolumeHandle(volumeID); err == nil {
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

func (ns *Server) NodeUnstageVolume(
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

// must be idempotent
//
//nolint:cyclop // many cases in switch increases complexity
func (ns *Server) stageVolume(
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

// restageVolume repairs a staging mount whose backing NVMe-oF device was lost
// (total path loss → the kernel removed the device, leaving a dead EIO mount).
// It force-unmounts the dead mount, reconnects the volume, and remounts the
// EXISTING filesystem in place. It never reformats — the volume already holds
// data. Filesystem (mount) volumes only; block volumes have no staging mount.
func (ns *Server) restageVolume(
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

// stagedFsType returns the filesystem a volume was staged with: the one
// recorded at stage time when it is there, and otherwise the one the volume
// capability asks for, which is all a volume staged by an older driver has.
func stagedFsType(volumeContext map[string]string, volCap *csi.VolumeCapability) string {
	if fsType := strings.TrimSpace(volumeContext[stagedFsTypeKey]); fsType != "" {
		return fsType
	}
	return fsTypeOrDefault(volCap)
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
