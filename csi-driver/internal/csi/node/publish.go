// Publishing: bind-mounting a staged volume into the path one workload reads
// it through, and the repair a publish performs on a stack that went away
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

	"github.com/simplyblock/atlas/volstack"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

func (ns *Server) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath()
	volumeContext, err := lookupVolumeContext(stagingParentPath)
	if err != nil {
		klog.Errorf("failed to retrieve volume context for volume %s: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	// A pNFS volume is bound straight from its staging mount: it has no volume
	// stack, so the plan below cannot be built for one.
	if isPNFSVolume(volumeContext) {
		if err := ns.publishPNFSVolume(ctx, req, getStagingTargetPath(req)); err != nil {
			klog.Errorf("failed to publish pNFS volume %s: %v", volumeID, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// One plan for the whole RPC: the heal below and the bind-mount after it act
	// on the same stack, and resolving where the volume is published twice would
	// be one control-plane round trip too many.
	plan, err := ns.attachPlan(
		ctx, volumeID, getStagingTargetPath(req), volumeContext, req.GetVolumeCapability())
	if err != nil {
		klog.Errorf("failed to build the stack plan for volume %s: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	// kubelet skips NodeStageVolume while the volume is still referenced on this
	// node, which a same-node pod replacement is, so this is the reliable place
	// to heal a stack whose foundation went away. Heal repairs what is broken
	// and creates nothing, so a healthy volume costs a read of each layer.
	if err := ns.healStack(ctx, volumeID, plan, stagingParentPath); err != nil {
		klog.Errorf("failed to heal volume %s before publish: %v", volumeID, err)
		return nil, status.Errorf(codes.Internal, "heal volume %s before publish: %v", volumeID, err)
	}

	if err := ns.publishVolume(ctx, plan, getStagingTargetPath(req), req); err != nil { // idempotent
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
func (ns *Server) publishVolume(
	ctx context.Context,
	plan volstack.Plan,
	stagingPath string,
	req *csi.NodePublishVolumeRequest,
) error {
	targetPath := req.GetTargetPath()

	fsType := req.GetVolumeCapability().GetMount().GetFsType()

	if req.GetVolumeCapability().GetBlock() != nil {
		devicePath, err := ns.stagedDevice(ctx, plan, req)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
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

// stagedDevice is the device a raw block volume is published from: what the
// live stack currently exposes, read rather than remembered.
//
// It is read because the answer changes. A reconnect produces a different
// namespace device, and the heal that ran before this publish may have just
// produced one. The stashed context is the fallback, for a volume staged before
// the stack existed and whose layers therefore report nothing.
func (ns *Server) stagedDevice(
	ctx context.Context,
	plan volstack.Plan,
	req *csi.NodePublishVolumeRequest,
) (string, error) {
	volumeID := req.GetVolumeId()

	artifact, err := ns.stack.runner.Observe(ctx, plan)
	if err != nil {
		klog.Warningf("volume %s: could not read what its stack exposes: %v", volumeID, err)
	} else if device, ok := artifact.Device(); ok {
		return device.Path, nil
	}

	volumeContext, err := lookupVolumeContext(req.GetStagingTargetPath())
	if err != nil {
		return "", fmt.Errorf("failed to retrieve volume context for volume %s: %w", volumeID, err)
	}
	if devicePath := volumeContext["devicePath"]; devicePath != "" {
		return devicePath, nil
	}
	return "", fmt.Errorf("could not find device path for volume %s", volumeID)
}

func getStagingTargetPath(req interface{}) string {
	switch vr := req.(type) {
	case *csi.NodeStageVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodeUnstageVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodePublishVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodeExpandVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	default:
		klog.Warningf("invalid request %T", vr)
	}
	return ""
}
