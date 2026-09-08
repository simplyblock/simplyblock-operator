// Publishing: bind-mounting a staged volume into the path one workload reads
// it through, and the repairs a publish performs on a staging that went away
// underneath it.
package node

import (
	"context"
	"fmt"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/initiator"
)

func (ns *Server) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	// If the backing NVMe-oF device was lost (total path loss), repair it before
	// bind-mounting into the pod, since otherwise the pod inherits the dead mount or
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
		ns.guardian.RegisterPublish(req.VolumeContext[csicommon.ParamClusterID], req.VolumeContext["uuid"], req.TargetPath)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *Server) NodeUnpublishVolume(
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
		if spdkVol, err := csicommon.ParseVolumeHandle(volumeID); err == nil {
			ns.guardian.RegisterUnpublish(spdkVol.VolumeID, req.TargetPath)
		} else {
			klog.Warningf("NodeUnpublishVolume: could not parse volume ID %q for Guardian tracking: %v", volumeID, err)
		}
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// must be idempotent
func (ns *Server) publishVolume(stagingPath string, req *csi.NodePublishVolumeRequest) error {
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

// healVolumeBeforePublish repairs a volume whose backing NVMe-oF device was lost
// (total path loss) before it is bind-mounted into a (replacement) pod. For
// filesystem volumes it restages the dead staging mount, and for block volumes it
// reconnects the missing device. No-op when the volume is healthy.
func (ns *Server) healVolumeBeforePublish(ctx context.Context, req *csi.NodePublishVolumeRequest) error {
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
func (ns *Server) ensureDeviceConnected(ctx context.Context, volumeID, stagingParentPath string) error {
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
