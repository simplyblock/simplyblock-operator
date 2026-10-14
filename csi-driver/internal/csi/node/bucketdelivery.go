// DeliverBucket and WithdrawBucket: making an S3 bucket's pNFS volume appear
// mounted inside an already-running gateway pod, by calling this node
// plugin's own NodeStageVolume/NodePublishVolume family in-process instead
// of through kubelet (design-bucket-provisioning.md §5.2,
// 0004-direct-csi-mount-spike-result.md). No new mount capability and no new
// persistence: both read and write the same staged-volume bookkeeping a
// kubelet-driven stage already would, so the plugin's existing restart
// recovery covers a bucket this way too.
//
// Registered as atlas/bucketdelivery/bucketdeliveryrpc's Deliverer on the
// csi-link session this plugin already holds open (internal/csilink), not on
// the kubelet-facing CSI socket: the operator is the only caller, the same
// way NFSExport assembly reaches a metadata server over that link rather
// than over a new one.

package node

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/simplyblock/atlas/bucketdelivery/bucketdeliveryrpc"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
)

// s3GatewayMountRoot is where a delivered bucket's mount is published: the
// one shared, fixed hostPath every gateway pod of every ObjectStore mounts
// HostToContainer (design §5.1). It is a subdirectory of
// /var/lib/kubelet/plugins, one of the three hostPath roots this DaemonSet's
// own pod template already mounts Bidirectional
// (0004-direct-csi-mount-spike-result.md). A target_path outside those
// three stays inside this container's own mount namespace and reaches
// neither the host nor any other pod.
const s3GatewayMountRoot = "/var/lib/kubelet/plugins/csi.simplyblock.io/s3-gateway-mounts"

// s3BucketStagingRoot is this plugin's own internal staging area for a
// delivered bucket's pNFS mount, bind-mounted from into s3GatewayMountRoot by
// NodePublishVolume. It does not itself need to be under a Bidirectional
// root: nothing but this container ever reads it directly.
const s3BucketStagingRoot = "/var/lib/kubelet/plugins/csi.simplyblock.io/s3-gateway-staging"

// bidirectionalRoots are the only three host paths this DaemonSet's pod
// template mounts Bidirectional, confirmed empirically rather than assumed
// (0004-direct-csi-mount-spike-result.md). Any delivery path this plugin
// derives must fall under one of them, or the resulting mount is invisible
// to every other pod while still looking like it succeeded.
var bidirectionalRoots = []string{
	"/var/lib/kubelet/plugins",
	"/var/lib/kubelet/pods",
	"/var/lib/nvme",
}

// validateDeliveryPath rejects a path outside every Bidirectional root
// before any mount is attempted, rather than mounting it invisibly inside
// this container's own namespace and reporting success
// (0004-direct-csi-mount-spike-result.md's finding). This is a guard against
// this file's own derivation ever drifting from s3GatewayMountRoot, not a
// check on caller input: bucket_name never reaches a path on its own.
func validateDeliveryPath(path string) error {
	clean := filepath.Clean(path)
	for _, root := range bidirectionalRoots {
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return nil
		}
	}
	return fmt.Errorf("bucketdelivery: target path %s is outside every Bidirectional hostPath root %v", path, bidirectionalRoots)
}

// bucketName validates the bucket name this RPC was asked to deliver.
// bucket.Name is admission-validated by the S3Bucket CRD's own pattern by
// the time an operator calls this, but this plugin trusts no caller's
// validation but its own before turning the value into a filesystem path.
func bucketName(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("bucketdelivery: refusing bucket name %q", name)
	}
	return name, nil
}

// bucketTargetPath is the deterministic, published mount path for a bucket:
// the same relative path under s3GatewayMountRoot regardless of which node
// or when it was delivered, which is what lets every gateway pod on a node
// see the same bucket at the same path (design §5.2).
func bucketTargetPath(name string) (string, error) {
	safe, err := bucketName(name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s3GatewayMountRoot, safe)
	if err := validateDeliveryPath(path); err != nil {
		return "", err
	}
	return path, nil
}

// bucketStagingPath is this plugin's own internal staging parent for a
// bucket, in the StagingTargetPath/VolumeId shape getStagingTargetPath
// expects (publish.go).
func bucketStagingPath(name string) (string, error) {
	safe, err := bucketName(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(s3BucketStagingRoot, safe), nil
}

// bucketVolumeCapability is the RWX mount capability every bucket delivery
// carries: MULTI_NODE_MULTI_WRITER is the one access mode a pNFS volume is
// ever bound with (design-pnfs-rwx.md), and bucket delivery is no exception.
func bucketVolumeCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
	}
}

// Deliver implements bucketdeliveryrpc.Deliverer: stage, then publish, the
// bucket's backing pNFS volume at its deterministic path. Idempotent,
// because the NodeStageVolume/NodePublishVolume calls it wraps already are:
// stagePNFS checks IsMounted, and publishPNFSVolume checks IsDead and
// EnsureDirectory.
func (ns *Server) Deliver(ctx context.Context, bucket bucketdeliveryrpc.Bucket) error {
	if proto := bucket.VolumeContext[csicommon.CtxAccessProtocol]; proto != csicommon.AccessProtocolNFS {
		return fmt.Errorf("bucketdelivery: bucket %s carries access_protocol %q, want %q",
			bucket.Name, proto, csicommon.AccessProtocolNFS)
	}
	targetPath, err := bucketTargetPath(bucket.Name)
	if err != nil {
		return err
	}
	stagingParent, err := bucketStagingPath(bucket.Name)
	if err != nil {
		return err
	}

	if _, err := ns.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          bucket.VolumeHandle,
		StagingTargetPath: stagingParent,
		VolumeContext:     bucket.VolumeContext,
		VolumeCapability:  bucketVolumeCapability(),
	}); err != nil {
		return fmt.Errorf("bucketdelivery: stage bucket %s: %w", bucket.Name, err)
	}

	if _, err := ns.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          bucket.VolumeHandle,
		StagingTargetPath: stagingParent,
		TargetPath:        targetPath,
		VolumeContext:     bucket.VolumeContext,
		VolumeCapability:  bucketVolumeCapability(),
	}); err != nil {
		return fmt.Errorf("bucketdelivery: publish bucket %s: %w", bucket.Name, err)
	}
	return nil
}

// Withdraw implements bucketdeliveryrpc.Deliverer: unpublish, then unstage.
//
// NodeUnpublishVolume is already idempotent (it removes a mount point that
// may already be gone). NodeUnstageVolume's pNFS branch is not, on its own:
// it tells a pNFS volume apart from an ordinary one by a stash file the
// first successful unstage deletes, so a second call would silently fall
// through to the wrong teardown path instead of finding nothing to do. This
// checks the staging mount directly first, which is what lets a repeated
// Withdraw converge instead of erroring on the mismatch.
func (ns *Server) Withdraw(ctx context.Context, bucket bucketdeliveryrpc.Bucket) error {
	targetPath, err := bucketTargetPath(bucket.Name)
	if err != nil {
		return err
	}
	stagingParent, err := bucketStagingPath(bucket.Name)
	if err != nil {
		return err
	}
	stagingPath := getStagingTargetPath(&csi.NodeUnstageVolumeRequest{
		VolumeId: bucket.VolumeHandle, StagingTargetPath: stagingParent,
	})

	if _, err := ns.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId: bucket.VolumeHandle, TargetPath: targetPath,
	}); err != nil {
		return fmt.Errorf("bucketdelivery: unpublish bucket %s: %w", bucket.Name, err)
	}

	mounted, err := ns.mounter.IsMounted(stagingPath)
	if err != nil {
		return fmt.Errorf("bucketdelivery: checking staging mount for bucket %s: %w", bucket.Name, err)
	}
	if !mounted {
		// A previous Withdraw already unstaged it. Nothing left to tear down.
		return nil
	}

	if _, err := ns.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: bucket.VolumeHandle, StagingTargetPath: stagingParent,
	}); err != nil {
		return fmt.Errorf("bucketdelivery: unstage bucket %s: %w", bucket.Name, err)
	}
	return nil
}
