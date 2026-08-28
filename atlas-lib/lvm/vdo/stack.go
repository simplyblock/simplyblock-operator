package vdo

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/simplyblock/atlas/lvm"
)

const (
	cmdTimeout  = 120 * time.Second
	growTimeout = 300 * time.Second

	volumeGroupPrefix = "vdo-"
	// poolName is the fixed name of the VDO pool LV within every volume's volume
	// group. It is not per-volume: uniqueness comes from the volume group name
	// (vdo-<lvolID>), one volume group per volume.
	poolName = "vdopool"
)

// Logger is used to emit non-fatal warnings: a best-effort rescan that
// failed, an unreachable-device fallback firing. When nil, slog.Default() is
// resolved at log time so it follows the application's configured default.
// This package stays free of any Kubernetes-specific logging dependency, so
// it is usable outside a Kubernetes context. A caller that wants its own log
// format sets Logger once at startup.
var Logger *slog.Logger

func warnf(format string, args ...any) {
	l := Logger
	if l == nil {
		l = slog.Default()
	}
	l.Warn(fmt.Sprintf(format, args...))
}

// volumeGroupName returns the volume group name lvolID's VDO stack lives in.
func volumeGroupName(lvolID string) string {
	return volumeGroupPrefix + lvolID
}

// DevicePath returns the path to lvolID's VDO logical volume (the device to
// format/mount, as opposed to the raw NVMe-oF device VDO sits on top of).
func DevicePath(lvolID string) string {
	return fmt.Sprintf("/dev/%s/%s", volumeGroupName(lvolID), lvolID)
}

// cmdCtx and growCtx bound one LVM/dm-vdo invocation. A field on lvm.Manager
// itself would have to pick one timeout for every call a Manager ever makes.
// Per-call context.WithTimeout keeps the same per-command granularity a quick
// identity probe and a multi-hundred-second lvextend need very different
// budgets for.
func cmdCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, cmdTimeout)
}

func growCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, growTimeout)
}

// CreateOrAttach idempotently ensures a VDO-backed logical volume exists on
// top of devicePath, named after lvolID, and returns the resulting device
// path to format/mount. If the volume group already exists it is reactivated
// and never recreated. Only a genuinely absent volume group is created fresh.
// compression and deduplication are set independently at creation time.
// Changing them on an already-existing volume is SetFeatures' job, not this
// function's.
func CreateOrAttach(
	ctx context.Context, manager *lvm.Manager, devicePath, lvolID string, compression, deduplication bool,
) (string, error) {
	vg := volumeGroupName(lvolID)

	// LVM discovers PVs by content-scanning visible devices, not by remembering a
	// path. Refresh its view before checking, since devicePath may have just
	// reappeared under a new device node (reconnect) or be genuinely new.
	rescanCtx, cancel := cmdCtx(ctx)
	if err := manager.Rescan(rescanCtx, devicePath); err != nil {
		warnf("vdo.CreateOrAttach: pvscan --cache %s: %v", devicePath, err)
	}
	cancel()

	existsCtx, cancel := cmdCtx(ctx)
	currentVG, err := manager.VolumeGroup(existsCtx, devicePath)
	cancel()
	if err != nil {
		return "", fmt.Errorf("probe volume group identity of %s: %w", devicePath, err)
	}

	if currentVG == vg {
		hasLVCtx, cancel := cmdCtx(ctx)
		hasLV, err := manager.HasLogicalVolume(hasLVCtx, vg, lvolID)
		cancel()
		if err != nil {
			return "", fmt.Errorf("check %s for logical volume %s: %w", vg, lvolID, err)
		}
		if hasLV {
			activateCtx, cancel := cmdCtx(ctx)
			err := manager.ActivateVolumeGroup(activateCtx, vg)
			cancel()
			if err != nil {
				return "", err
			}
			return DevicePath(lvolID), nil
		}
		// Orphaned volume group from an earlier interrupted create: pvcreate+vgcreate
		// completed but lvcreate never did, so there is nothing here to reactivate.
		// Every subsequent stage attempt would otherwise reactivate an empty volume
		// group forever, never producing a mountable device. Remove it and fall
		// through to a fresh create.
		warnf(
			"vdo.CreateOrAttach: volume group %s exists but has no %s logical volume, removing orphaned volume group and recreating", //nolint:lll
			vg, lvolID,
		)
		vgremoveCtx, cancel := cmdCtx(ctx)
		err = manager.RemoveVolumeGroup(vgremoveCtx, vg)
		cancel()
		if err != nil {
			return "", err
		}
	}

	pvcreateCtx, cancel := cmdCtx(ctx)
	err = manager.CreatePhysicalVolume(pvcreateCtx, devicePath)
	cancel()
	if err != nil {
		return "", err
	}
	vgcreateCtx, cancel := cmdCtx(ctx)
	err = manager.CreateVolumeGroup(vgcreateCtx, vg, devicePath)
	cancel()
	if err != nil {
		return "", err
	}
	lvcreateCtx, cancel := cmdCtx(ctx)
	def := lvm.LogicalVolumeDefinition{Compression: compression, Deduplication: deduplication}
	err = manager.CreateLogicalVolume(lvcreateCtx, vg, poolName, lvolID, def)
	cancel()
	if err != nil {
		return "", err
	}

	return DevicePath(lvolID), nil
}

// ResolveClone handles the case where devicePath is a block-level CoW
// clone/snapshot restore of another VDO-formatted volume: a byte-copy
// carries the source's PV/VG UUIDs and VG name verbatim, which
// CreateOrAttach's own "does vdo-<lvolID> exist" check cannot detect (the
// volume group on disk is still named after the source). Must run before
// CreateOrAttach whenever the volume was provisioned from a clone or
// snapshot source.
func ResolveClone(ctx context.Context, manager *lvm.Manager, devicePath, lvolID string) error {
	resolveCtx, cancel := cmdCtx(ctx)
	defer cancel()
	foreignVG, err := manager.ResolveClonedVolumeGroup(resolveCtx, devicePath, volumeGroupName(lvolID), lvolID, poolName)
	if err != nil {
		return err
	}
	if foreignVG != "" {
		warnf(
			"vdo.ResolveClone: device %s for volume %s carried a foreign volume group identity %q, regenerated PV/VG UUIDs and renamed to %s", //nolint:lll
			devicePath, lvolID, foreignVG, volumeGroupName(lvolID),
		)
	}
	return nil
}

// Deactivate deactivates (but does not destroy) the volume group/logical
// volume stack for lvolID. This is the correct, non-destructive counterpart
// to a plain NVMe-oF disconnect: a routine unstage fires any time nothing on
// this node currently needs the volume mounted, not only when the volume is
// actually being deleted. Calling the destructive Remove from that path
// destroys a volume's VDO metadata/data on an ordinary pod recreate, well
// before the volume was ever meant to be removed. CreateOrAttach's own
// reactivation path reaches this exact state later without recreating
// anything.
func Deactivate(ctx context.Context, manager *lvm.Manager, lvolID string) error {
	vg := volumeGroupName(lvolID)
	deactivateCtx, cancel := cmdCtx(ctx)
	err := manager.DeactivateVolumeGroup(deactivateCtx, vg)
	cancel()
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "Failed to find") {
		return err
	}

	// The volume group's metadata lives on the now-unreachable backing device, so
	// deactivating it safely is impossible: there is nothing left to read.
	// Falling back to a direct dmsetup removal here is safe, not destructive: it
	// only clears this host's own dead references to a device that has already
	// vanished, not the actual VDO metadata/data on the still-existing, just
	// currently unreachable, backend volume. Without this fallback, an unstage
	// fails indefinitely and the orphaned dm-vdo stack is never cleaned up.
	warnf(
		"vdo.Deactivate: deactivate %s failed (backing device unreachable), falling back to direct dmsetup removal of any live dm nodes", //nolint:lll
		vg,
	)
	removeCtx, cancel := cmdCtx(ctx)
	defer cancel()
	return manager.RemoveOrphanedDMNodes(removeCtx, vg)
}

// Remove deactivates and removes the volume group/logical volume stack for
// lvolID, destroying its data. Only appropriate when the underlying volume
// itself is actually being removed, or to clean up a stale/orphaned stack
// whose backing device is already gone. Never call this from a routine
// unstage. If the backing device is unreachable (crash / force-detach
// without a clean unstage), normal removal fails because it needs to
// read/write metadata that lives on the now-gone device, so this falls back
// to direct dmsetup removal of any live dm nodes in that case.
func Remove(ctx context.Context, manager *lvm.Manager, lvolID string) error {
	vg := volumeGroupName(lvolID)
	deactivateCtx, cancel := cmdCtx(ctx)
	_ = manager.DeactivateVolumeGroup(deactivateCtx, vg)
	cancel()

	removeCtx, cancel := cmdCtx(ctx)
	err := manager.RemoveVolumeGroup(removeCtx, vg)
	cancel()
	if err == nil {
		return nil
	}

	warnf(
		"vdo.Remove: remove %s failed (backing device likely unreachable), falling back to direct dmsetup removal of any live dm nodes", //nolint:lll
		vg,
	)
	dmCtx, cancel := cmdCtx(ctx)
	defer cancel()
	return manager.RemoveOrphanedDMNodes(dmCtx, vg)
}

// Grow extends the volume group's pool LV to consume all newly available
// physical space on devicePath (matching the 100%FREE convention used at
// creation time), then grows the VDO logical volume's own size to match, and
// returns its device path.
func Grow(ctx context.Context, manager *lvm.Manager, devicePath, lvolID string) (string, error) {
	vg := volumeGroupName(lvolID)

	pvresizeCtx, cancel := cmdCtx(ctx)
	err := manager.ExpandPhysicalVolume(pvresizeCtx, devicePath)
	cancel()
	if err != nil {
		return "", err
	}
	// lvextend's -l (unlike lvcreate's) treats a bare "100%FREE" as an ABSOLUTE
	// target size (100% of what's currently free), not "grow by," since
	// free-space-alone is smaller than the pool's current size. ExpandLogicalVolume's
	// "+" prefix makes it additive (current size + free space), which is what "grow
	// to consume all newly available space" means.
	lvextendPoolCtx, cancel := growCtx(ctx)
	err = manager.ExpandLogicalVolume(lvextendPoolCtx, vg, poolName)
	cancel()
	if err != nil {
		return "", err
	}

	// The VDO logical volume's size is independent of the pool's physical size (it
	// can safely be sized up to the pool's own capacity without relying on
	// dedup/compression savings, matching how creation omits -V to get the same
	// "largest safe size" default). Read the pool's new size back and extend the
	// logical volume to match it.
	sizeCtx, cancel := cmdCtx(ctx)
	poolSize, err := manager.LogicalVolumeSize(sizeCtx, vg, poolName)
	cancel()
	if err != nil {
		return "", fmt.Errorf("read grown pool size for %s/%s: %w", vg, poolName, err)
	}
	lvextendLVCtx, cancel := growCtx(ctx)
	err = manager.ExtendLogicalVolumeToSize(lvextendLVCtx, vg, lvolID, poolSize)
	cancel()
	if err != nil {
		return "", err
	}
	return DevicePath(lvolID), nil
}

// SetFeatures toggles compression/deduplication on lvolID's existing, active
// VDO volume without recreating it. A thin lvolID-keyed wrapper over
// UpdateVolume, for a caller (every other function in this file) that has
// only lvolID, not a *Volume.
func SetFeatures(ctx context.Context, manager *lvm.Manager, lvolID string, compression, deduplication bool) error {
	timeoutCtx, cancel := cmdCtx(ctx)
	defer cancel()
	volume := NewVolume(volumeGroupName(lvolID), poolName, lvolID)
	return UpdateVolume(timeoutCtx, manager, volume, compression, deduplication)
}
