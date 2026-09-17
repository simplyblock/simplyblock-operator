// The pNFS client path: what NodeStageVolume does when the volume is an export
// rather than a block device.
//
// The shape is the ordinary one plus a device alias. The namespace is attached
// exactly as it is for a block volume, because a pNFS client is an NVMe-oF
// initiator for the same namespace the MDS made the filesystem on -- that is
// the whole point, and it is why the data path bypasses the metadata server.
// What is added is a name: the kernel builds a /dev/disk/by-id path from the
// designator the MDS advertises, and nothing creates that path on its own.
//
// Then an ordinary NFSv4.1 mount, and the kernel does the rest: LAYOUTGET on
// first I/O, then reads and writes straight to the namespace.

package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"

	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nqn"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"

	"github.com/simplyblock/csi-driver/internal/nfsexport"
)

// aliasDir is where the kernel looks for the device the layout names.
const aliasDir = "/dev/disk/by-id"

// aliasPrefix is the one of the three prefixes bl_parse_scsi tries that applies
// to an NVMe namespace.
//
// It is `nvme-eui.`, not `nvme-eui64.`. The kernel builds the path itself as
// "/dev/disk/by-id/%s%*phN" and tries dm-uuid-mpath-0x, then wwn-0x, then this
// one; a name that matches none of them leaves the client unable to map the
// layout, and the failure is silent -- it returns the layout and falls back to
// routing every byte through the metadata server, which looks exactly like
// working.
const aliasPrefix = "nvme-eui."

// nfsMountOptions are the options every pNFS mount carries. 4.1 is the floor:
// layouts do not exist before it.
var nfsMountOptions = []string{"vers=4.1"}

// aliasPath is where the alias for a namespace goes.
//
// The NGUID is lowercased and stripped of punctuation because the two places it
// comes from disagree: sysfs reports it hyphenated, `nvme id-ns` reports it
// bare, and the kernel formats the designator as plain lowercase hex. A name
// built from the wrong spelling matches nothing.
func aliasPath(nguid string) string {
	return filepath.Join(aliasDir, aliasPrefix+normalizeNGUID(nguid))
}

// normalizeNGUID reduces an NGUID to the spelling the kernel builds its lookup
// path from: lowercase hex, no separators.
func normalizeNGUID(nguid string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(nguid) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ensureDeviceAlias creates the by-id name the client kernel resolves the
// layout through, pointing at the local block device for the namespace.
//
// It is idempotent, and it replaces an alias pointing somewhere else rather than
// leaving it: a device node is assigned in attach order, so the same namespace
// can be a different path after a reconnect, and a stale alias would send the
// kernel at whatever now holds the old path.
func ensureDeviceAlias(nguid, devicePath string) (string, error) {
	if normalizeNGUID(nguid) == "" {
		return "", fmt.Errorf("pnfs: namespace has no usable NGUID (%q)", nguid)
	}
	if devicePath == "" {
		return "", fmt.Errorf("pnfs: no device path for namespace %s", nguid)
	}
	alias := aliasPath(nguid)
	if err := os.MkdirAll(aliasDir, 0o755); err != nil {
		return "", fmt.Errorf("pnfs: creating %s: %w", aliasDir, err)
	}

	if existing, err := os.Readlink(alias); err == nil {
		if existing == devicePath {
			return alias, nil
		}
		// Pointing elsewhere: replace rather than leave, per above.
		if err := os.Remove(alias); err != nil {
			return "", fmt.Errorf("pnfs: replacing stale alias %s: %w", alias, err)
		}
	}
	if err := os.Symlink(devicePath, alias); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("pnfs: linking %s to %s: %w", alias, devicePath, err)
	}
	return alias, nil
}

// removeDeviceAlias drops the alias, and treats an absent one as done.
func removeDeviceAlias(nguid string) error {
	if normalizeNGUID(nguid) == "" {
		return nil
	}
	if err := os.Remove(aliasPath(nguid)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pnfs: removing alias for %s: %w", nguid, err)
	}
	return nil
}

// nfsSource is what gets mounted: the export's address and the path the MDS
// exports it at.
func nfsSource(serviceAddress, exportPath string) string {
	return serviceAddress + ":" + exportPath
}

// mountOptions merges the class's extra options after the ones every pNFS mount
// needs, so a class can add but not silently drop the version that makes
// layouts possible.
func mountOptions(extra string) []string {
	opts := append([]string{}, nfsMountOptions...)
	for _, o := range strings.Split(extra, ",") {
		if o = strings.TrimSpace(o); o != "" {
			opts = append(opts, o)
		}
	}
	return opts
}

// stagePNFS attaches the export at the staging path.
//
// The mounter is the host's, not the driver's own. mount(8) hands an NFS mount
// to /sbin/mount.nfs, a helper from nfs-utils that this image does not carry --
// and a host that may run a ReadWriteMany pod needs nfs-utils anyway, so
// shipping a second copy would be two things to keep in step. Mounting on the
// host also puts the mount straight where kubelet looks instead of relying on
// it propagating out of the container.
//
// Nothing here formats. The filesystem was made by the metadata server, and a
// client that could format one would be a client that could destroy it.
func stagePNFS(
	ctx context.Context,
	mounter nfsexport.HostMounter,
	stagingPath, serviceAddress, exportPath, nguid, devicePath, extraOptions string,
) error {
	if _, err := ensureDeviceAlias(nguid, devicePath); err != nil {
		// Without the alias the mount still succeeds and every byte silently
		// routes through the metadata server, so this is a failure rather than
		// a warning: a working-looking mount with none of the throughput the
		// feature exists for is worse than a refused one.
		return err
	}
	mounted, err := mounter.IsMountPoint(ctx, stagingPath)
	if err != nil {
		return fmt.Errorf("pnfs: checking %s: %w", stagingPath, err)
	}
	if mounted {
		return nil
	}
	// Made in this container, which is the host's directory: the staging tree
	// is a bidirectionally propagated hostPath, so the host mount below has
	// somewhere to land.
	if err := os.MkdirAll(stagingPath, 0o750); err != nil {
		return fmt.Errorf("pnfs: creating %s: %w", stagingPath, err)
	}
	source := nfsSource(serviceAddress, exportPath)
	if err := mounter.Mount(ctx, source, stagingPath, "nfs", mountOptions(extraOptions)); err != nil {
		return fmt.Errorf("pnfs: mounting %s at %s: %w", source, stagingPath, err)
	}
	return nil
}

// unstagePNFS detaches the export and drops the alias.
func unstagePNFS(
	ctx context.Context, mounter nfsexport.HostMounter, stagingPath, nguid string,
) error {
	mounted, err := mounter.IsMountPoint(ctx, stagingPath)
	if err != nil {
		return fmt.Errorf("pnfs: checking %s: %w", stagingPath, err)
	}
	if mounted {
		if err := mounter.Unmount(ctx, stagingPath); err != nil {
			return fmt.Errorf("pnfs: unmounting %s: %w", stagingPath, err)
		}
	}
	return removeDeviceAlias(nguid)
}

// VolumeContext keys the controller writes for a pNFS volume. They are read
// only here, and nothing else produces them.
const (
	ctxAccessProtocol = "access_protocol"
	ctxExportService  = "export_service"
	ctxExportPath     = "export_path"
	ctxNGUID          = "nguid"
	ctxMountOptions   = "nfs_mount_options"

	accessProtocolNFS = "nfs"
)

// backingVolumeOf reads the backing namespace out of a pNFS volume handle, in
// the shape the attacher wants it.
//
// Only the four-part `nfs:` form is accepted. A block handle has one field
// fewer, so reading it here would shift every field and attach whatever the
// shifted values happened to name.
func backingVolumeOf(volumeHandle string) (export.Spec, bool) {
	handle, ok := lvol.ParseNFSHandle(lvol.VolumeHandle(volumeHandle))
	if !ok {
		return export.Spec{}, false
	}
	// The namespace UUID of a simplyblock volume is the logical volume's own
	// id, which is what the handle's export UUID carries.
	return export.Spec{
		VolumeUUID: handle.ExportUUID,
		ClusterID:  handle.ClusterID,
		PoolID:     handle.PoolRef,
	}, true
}

// stagePNFSVolume connects the backing namespace and mounts the export.
//
// The connect is not optional and not somebody else's: a pNFS client is an
// NVMe-oF initiator for the same namespace the MDS made the filesystem on --
// that is what lets the data path bypass the metadata server -- and the block
// branch of NodeStageVolume is the other branch, so for a pNFS volume it never
// runs. Without this the mount still succeeds, the client finds no local device
// for the layout, and every byte routes through the metadata server, which
// looks exactly like working.
func (ns *Server) stagePNFSVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
	stagingTargetPath string,
) error {
	volumeContext := req.GetVolumeContext()
	service := volumeContext[ctxExportService]
	exportPath := volumeContext[ctxExportPath]
	if service == "" || exportPath == "" {
		return fmt.Errorf(
			"pnfs: volume %s has no export address yet (service=%q path=%q)",
			req.GetVolumeId(), service, exportPath)
	}

	spec, ok := backingVolumeOf(req.GetVolumeId())
	if !ok {
		return fmt.Errorf("pnfs: %q is not a pNFS volume handle", req.GetVolumeId())
	}
	if err := ns.attachBacking(ctx, spec); err != nil {
		return err
	}

	// The NGUID is read from the attached device rather than taken from the
	// volume context, because it is assigned by the target: only a host with
	// the namespace attached can know it, and the controller never has one.
	nguid, devicePath, err := ns.namespaceIdentity(ctx, req.GetVolumeId(), volumeContext[ctxNGUID])
	if err != nil {
		return err
	}

	return stagePNFS(ctx, nfsexport.HostFilesystem(), stagingTargetPath,
		service, exportPath, nguid, devicePath, volumeContext[ctxMountOptions])
}

// attachBacking connects the namespace behind a pNFS volume, through the same
// path the export service uses on the metadata-server host. One implementation
// serves both, so a client and a server cannot disagree about how a namespace
// is connected or under which host identity.
func (ns *Server) attachBacking(ctx context.Context, spec export.Spec) error {
	return nfsexport.Attach(ctx, spec, ns.hostNQN)
}

// detachBacking gives it up again, after the export has been unmounted.
func (ns *Server) detachBacking(ctx context.Context, spec export.Spec) error {
	return nfsexport.Detach(ctx, spec, ns.hostNQN)
}

// hostNQN is this node's NVMe qualified name, derived from the Kubernetes
// node's UID exactly as the block staging path derives it, so the control plane
// sees one identity for this host however the namespace was connected.
func (ns *Server) hostNQN(ctx context.Context) string {
	if ns.kubeClient == nil {
		return ""
	}
	nodeName := ns.Driver.GetNodeID()
	node, err := ns.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("pnfs: reading node %s for the host NQN: %v", nodeName, err)
		return ""
	}
	return nqn.Host(string(node.UID))
}

// namespaceIdentity finds the locally attached namespace for a pNFS volume and
// reports the NGUID the kernel will name it by, along with its device node.
func (ns *Server) namespaceIdentity(
	ctx context.Context,
	volumeHandle, hintedNGUID string,
) (nguid, devicePath string, err error) {
	handle, ok := lvol.ParseNFSHandle(lvol.VolumeHandle(volumeHandle))
	if !ok {
		return "", "", fmt.Errorf("pnfs: %q is not a pNFS volume handle", volumeHandle)
	}
	// The namespace UUID of a simplyblock volume is the logical volume's own
	// id, which is what the handle carries.
	device, err := storage.Local(nvme.SysfsConfig{}).DeviceByUUID(ctx, handle.ExportUUID)
	if err != nil {
		return "", "", fmt.Errorf("pnfs: finding the namespace for %s: %w", handle.ExportUUID, err)
	}
	nguid = device.Namespace.NGUID
	if nguid == "" {
		// A hint from the record is better than nothing, but a namespace that
		// publishes no NGUID cannot be mapped at all and saying so is more
		// useful than mounting something that will route through the MDS.
		nguid = hintedNGUID
	}
	return nguid, device.Namespace.DevicePath, nil
}

// unstagePNFSVolume unmounts the export, drops the device alias, and gives the
// namespace back.
//
// The order is the reverse of staging and it matters: detaching under a live
// mount leaves a filesystem over a device that is gone, which is an EIO every
// process in it has to be killed to clear.
//
// The NGUID is read before the unmount, because the alias is named by it and
// the device is what reports it: once the namespace is detached there is
// nothing left to ask, and the alias would be left behind pointing at a device
// node the next attach may hand to something else.
func (ns *Server) unstagePNFSVolume(
	ctx context.Context, stagingTargetPath string, spec export.Spec,
) error {
	var nguid string
	if device, err := storage.Local(nvme.SysfsConfig{}).DeviceByUUID(ctx, spec.VolumeUUID); err == nil {
		nguid = device.Namespace.NGUID
	}
	if err := unstagePNFS(ctx, nfsexport.HostFilesystem(), stagingTargetPath, nguid); err != nil {
		return err
	}
	return ns.detachBacking(ctx, spec)
}

// isPNFSVolume is the cheap question every path that branches on volume kind
// asks. It reads the handle rather than the volume context, because the handle
// is the one thing every node RPC is given.
func isPNFSVolume(volumeHandle string) bool {
	return lvol.VolumeHandle(volumeHandle).IsNFS()
}

// restagePNFSVolume repairs a dead pNFS staging mount.
//
// It takes the volume context from the request rather than from a stash on
// disk, because a pNFS stage writes none: everything it needs is the handle and
// the context kubelet passes on every call, so there was nothing to stash. The
// generic repair path reads that stash and so cannot repair one of these, which
// is why this exists rather than the two sharing.
func (ns *Server) restagePNFSVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
	stagingTargetPath string,
) error {
	klog.Warningf("restaging pNFS volume %s: staging mount %s is dead",
		req.GetVolumeId(), stagingTargetPath)
	if err := ns.mounter.Remove(stagingTargetPath); err != nil {
		return fmt.Errorf("pnfs: clearing the dead mount at %s: %w", stagingTargetPath, err)
	}
	return ns.stagePNFSVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:      req.GetVolumeId(),
		VolumeContext: req.GetVolumeContext(),
	}, stagingTargetPath)
}

// publishFSType is the filesystem type a bind mount into the pod is given.
//
// For a pNFS volume it is nothing. Publishing is a bind of an already-mounted
// path, and a bind takes no type; the type on the volume capability comes from
// the StorageClass's csi.storage.k8s.io/fstype, which for a ReadWriteMany
// volume describes the filesystem the metadata server makes rather than what
// this client mounted. Passing it on makes mount(8) look for a helper named
// after the type, and /sbin/mount.nfs exists and does not bind.
func publishFSType(volumeHandle, requested string) string {
	if isPNFSVolume(volumeHandle) {
		return ""
	}
	return requested
}
