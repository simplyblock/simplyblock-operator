// The pNFS client path: what NodeStageVolume does for an export.
//
// A client is an NVMe-oF initiator for the same namespace the MDS made the
// filesystem on, which is why the data path bypasses the metadata server. So:
// the ordinary connect, plus a device alias the kernel resolves the layout
// through, plus an NFSv4.1 mount.

package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"k8s.io/klog"

	"github.com/simplyblock/atlas/export"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/nvme"
	"github.com/simplyblock/atlas/storage"

	csicommon "github.com/simplyblock/csi-driver/internal/csi/common"
	"github.com/simplyblock/csi-driver/internal/nfsexport"
)

// aliasDir is where the kernel looks for the device the layout names.
const aliasDir = "/dev/disk/by-id"

// aliasPrefix is `nvme-eui.`, not `nvme-eui64.`. The kernel builds the path as
// "/dev/disk/by-id/%s%*phN" and tries dm-uuid-mpath-0x, wwn-0x, then this. A
// name matching none of them fails silently: the layout is returned and every
// byte routes through the metadata server, which looks exactly like working.
const aliasPrefix = "nvme-eui."

// nfsMountOptions are the options every pNFS mount carries. 4.1 is the floor:
// layouts do not exist before it.
var nfsMountOptions = []string{"vers=4.1"}

// aliasPath is where the alias for a namespace goes.
func aliasPath(nguid string) string {
	return filepath.Join(aliasDir, aliasPrefix+normalizeNGUID(nguid))
}

// normalizeNGUID reduces an NGUID to the spelling the kernel looks for:
// lowercase hex, no separators. sysfs reports it hyphenated and `nvme id-ns`
// reports it bare, so a name built from the wrong one matches nothing.
func normalizeNGUID(nguid string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(nguid) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ensureDeviceAlias creates the by-id name the client resolves the layout
// through. Idempotent, and it replaces a stale alias rather than leaving it:
// device nodes are assigned in attach order, so a reconnect can move one.
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

// nfsMounter is the mounting a pNFS client does, which is the driver's own
// mounter narrowed to it. The mount runs in this container and reaches the node
// through the bidirectional propagation on the staging tree.
type nfsMounter interface {
	Mount(source, target, fsType string, flags []string) error
	Unmount(target string) error
	IsMounted(path string) (bool, error)
}

// stagePNFS mounts the export at the staging path. Nothing here formats -- the
// filesystem is the metadata server's, and a client that could make one could
// destroy it.
func stagePNFS(
	ctx context.Context,
	mounter nfsMounter,
	stagingPath, serviceAddress, exportPath, nguid, devicePath string,
) error {
	if _, err := ensureDeviceAlias(nguid, devicePath); err != nil {
		// Without the alias the mount succeeds and every byte routes through
		// the metadata server. A refused mount beats a slow invisible one.
		return err
	}
	mounted, err := mounter.IsMounted(stagingPath)
	if err != nil {
		return fmt.Errorf("pnfs: checking %s: %w", stagingPath, err)
	}
	if mounted {
		return nil
	}
	if err := os.MkdirAll(stagingPath, 0o750); err != nil {
		return fmt.Errorf("pnfs: creating %s: %w", stagingPath, err)
	}
	source := nfsSource(serviceAddress, exportPath)
	if err := mounter.Mount(source, stagingPath, "nfs", nfsMountOptions); err != nil {
		return fmt.Errorf("pnfs: mounting %s at %s: %w", source, stagingPath, err)
	}

	// Before any pod can. See primeLayout.
	if err := primeLayout(ctx, stagingPath); err != nil {
		// Not fatal: the volume is usable, only the direct path is lost. Loud,
		// because the failure is otherwise invisible.
		klog.Warningf(
			"pnfs: could not take the block layout for %s, so I/O will route through "+
				"the metadata server instead of going direct: %v", stagingPath, err)
	}
	return nil
}

// primeLayoutFile is removed immediately; the name is for a crash that leaves
// one behind.
const primeLayoutFile = ".simplyblock-pnfs-layout-probe"

// primeLayout triggers the first LAYOUTGET, from this process rather than a pod.
//
// The client resolves a layout's device in the mount namespace of whichever
// process caused the I/O, and a pod's /dev is kubelet's minimal one with no
// disk/. So a layout the application asks for first can never resolve -- and
// it sticks: the device is marked unavailable for two minutes and the
// read-write fail bit is set, so everything after routes through the MDS.
//
// This container has the host's /dev. Resolving here leaves the device in the
// per-client cache, so one touch covers every file a pod later opens. It
// writes rather than reads because a layout is per-inode.
func primeLayout(ctx context.Context, stagingPath string) error {
	// Checked before rather than during: the syscalls below are not
	// cancellable, and a stage that gave up should not add I/O.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pnfs: not taking the layout for %s: %w", stagingPath, err)
	}

	probe := filepath.Join(stagingPath, primeLayoutFile)
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("pnfs: opening the layout probe at %s: %w", probe, err)
	}
	// Removed whatever happens: this is the user's filesystem.
	defer func() {
		_ = f.Close()
		_ = os.Remove(probe)
	}()

	// One block, enough to ask for a read-write layout.
	if _, err := f.Write(make([]byte, 4096)); err != nil {
		return fmt.Errorf("pnfs: writing the layout probe: %w", err)
	}
	// Synced: the layout is taken on write-back, not on entering the page
	// cache, and the ordering against pod I/O depends on it.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("pnfs: syncing the layout probe: %w", err)
	}
	return nil
}

// unstagePNFS detaches the export and drops the alias.
func unstagePNFS(
	mounter nfsMounter, stagingPath, nguid string,
) error {
	mounted, err := mounter.IsMounted(stagingPath)
	if err != nil {
		return fmt.Errorf("pnfs: checking %s: %w", stagingPath, err)
	}
	if mounted {
		if err := mounter.Unmount(stagingPath); err != nil {
			return fmt.Errorf("pnfs: unmounting %s: %w", stagingPath, err)
		}
	}
	return removeDeviceAlias(nguid)
}

// backingVolumeOf reads the namespace a volume is served from, in the shape the
// attacher wants it. A simplyblock namespace UUID is the logical volume's own
// id, which is what the handle carries.
func backingVolumeOf(volumeHandle string) (export.Spec, bool) {
	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(volumeHandle))
	if !ok {
		return export.Spec{}, false
	}
	return export.Spec{
		VolumeUUID: handle.VolumeID,
		ClusterID:  handle.ClusterID,
		PoolID:     handle.PoolRef,
	}, true
}

// stagePNFSVolume connects the backing namespace and mounts the export.
//
// The connect is this path's own: the block branch of NodeStageVolume is the
// one a pNFS volume does not take, so nothing else runs it.
func (ns *Server) stagePNFSVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
	stagingTargetPath string,
) error {
	volumeContext := req.GetVolumeContext()
	service := volumeContext[csicommon.CtxExportService]
	exportPath := volumeContext[csicommon.CtxExportPath]
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

	// Read from the attached device, the only place it can come from: the
	// target assigns it.
	nguid, devicePath, err := ns.namespaceIdentity(ctx, req.GetVolumeId())
	if err != nil {
		return err
	}

	if err := stagePNFS(ctx, ns.mounter, stagingTargetPath,
		service, exportPath, nguid, devicePath); err != nil {
		return err
	}

	// Stashed like every other volume's, so unstage and publish can tell what
	// they are holding without reading it out of the handle.
	return stashVolumeContext(volumeContext, req.GetStagingTargetPath())
}

// attachBacking connects the namespace, through the same path the metadata
// server uses, so the two cannot disagree about how or under which identity.
func (ns *Server) attachBacking(ctx context.Context, spec export.Spec) error {
	return nfsexport.Attach(ctx, spec, ns.hostNQN())
}

// detachBacking gives it up again, after the export has been unmounted.
func (ns *Server) detachBacking(ctx context.Context, spec export.Spec) error {
	return nfsexport.Detach(ctx, spec, ns.hostNQN())
}

// hostNQN is the one derivation, shared with the metadata-server side.
func (ns *Server) hostNQN() nfsexport.HostNQNFunc {
	return nfsexport.HostNQN(ns.Driver.GetNodeID(), ns.kubeClient)
}

// namespaceIdentity finds the attached namespace and reports its NGUID and
// device node.
func (ns *Server) namespaceIdentity(
	ctx context.Context,
	volumeHandle string,
) (nguid, devicePath string, err error) {
	handle, ok := lvol.ParseHandle(lvol.VolumeHandle(volumeHandle))
	if !ok {
		return "", "", fmt.Errorf("pnfs: %q is not a volume handle", volumeHandle)
	}
	device, err := storage.Local(nvme.SysfsConfig{}).DeviceByUUID(ctx, handle.VolumeID)
	if err != nil {
		return "", "", fmt.Errorf("pnfs: finding the namespace for %s: %w", handle.VolumeID, err)
	}
	return device.Namespace.NGUID, device.Namespace.DevicePath, nil
}

// unstagePNFSVolume unmounts the export, drops the alias, and detaches.
//
// Staging's order reversed, and it matters: detaching under a live mount
// leaves a filesystem over a device that is gone. The NGUID is read first
// because after detaching there is nothing left to ask.
func (ns *Server) unstagePNFSVolume(
	ctx context.Context, stagingTargetPath string, spec export.Spec,
) error {
	var nguid string
	if device, err := storage.Local(nvme.SysfsConfig{}).DeviceByUUID(ctx, spec.VolumeUUID); err == nil {
		nguid = device.Namespace.NGUID
	}
	if err := unstagePNFS(ns.mounter, stagingTargetPath, nguid); err != nil {
		return err
	}
	return ns.detachBacking(ctx, spec)
}

// isPNFSVolume reads the stash a pNFS stage leaves. The handle does not say:
// it is the backing volume's, so a snapshot or a clone addresses the lvol
// without knowing an export is in front of it.
func isPNFSVolume(volumeContext map[string]string) bool {
	return volumeContext[csicommon.CtxAccessProtocol] == csicommon.AccessProtocolNFS
}

// restagePNFSVolume repairs a dead pNFS staging mount. It takes the volume
// context from the request, because a pNFS stage writes no stash.
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

// publishPNFSVolume binds the staged export into the pod. No volume stack, and
// no filesystem type: a bind takes none, and the capability carries the pnfs
// fsType, which would send mount(8) looking for a helper named after it.
func (ns *Server) publishPNFSVolume(
	ctx context.Context, req *csi.NodePublishVolumeRequest, stagingPath string,
) error {
	// kubelet skips NodeStageVolume while the volume is still referenced on
	// this node, so this is the reliable place to notice that the export's
	// mount died under it. Binding a dead mount into the pod would give it a
	// directory every read returns EIO from.
	if ns.mounter.IsDead(stagingPath) {
		if err := ns.restagePNFSVolume(ctx, req, stagingPath); err != nil {
			return err
		}
	}

	target := req.GetTargetPath()
	mounted, err := ns.mounter.EnsureDirectory(target)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	flags := append(req.GetVolumeCapability().GetMount().GetMountFlags(), "bind")
	return ns.mounter.Mount(stagingPath, target, "", flags)
}
