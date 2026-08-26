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

package util

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/simplyblock/atlas/lvm"
	"k8s.io/klog"
)

const (
	vdoCmdTimeoutSeconds  = 120
	vdoGrowTimeoutSeconds = 300
	vgPrefix              = "vdo-"
	// poolLVName is the fixed name of the VDO pool LV within every volume's VG. It is not
	// per-volume -- uniqueness comes from the VG name (vdo-<lvolID>), matching the design's
	// one-VG-per-volume convention.
	poolLVName = "vdopool"
)

// lvmManager runs every LVM/dm-vdo command in this file, scoped to a device and
// content-identity-aware where it matters -- see github.com/simplyblock/atlas/lvm's package
// doc comment for why: a simplyblock NVMe-oF HA volume's two redundant local device nodes
// present byte-identical content, which an unscoped, name-based LVM lookup cannot tell apart.
var lvmManager = lvm.NewManager()

// vgName returns the VG name this volume's VDO stack lives in.
func vgName(lvolID string) string {
	return vgPrefix + lvolID
}

// vdoDevicePath returns the path to the VDO logical volume -- the device to format/mount,
// as opposed to devicePath (the raw NVMe-oF device VDO sits on top of).
func vdoDevicePath(lvolID string) string {
	return fmt.Sprintf("/dev/%s/%s", vgName(lvolID), lvolID)
}

// withVDOTimeout and withVDOGrowTimeout bound one LVM/dm-vdo invocation. A field on
// lvm.Manager's Runner would have to pick one timeout for every call a Manager ever
// makes; per-call context.WithTimeout keeps the same per-command granularity the original
// direct-exec implementation had (a quick identity probe and a multi-hundred-second
// lvextend need very different budgets).
func withVDOTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, vdoCmdTimeoutSeconds*time.Second)
}

func withVDOGrowTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, vdoGrowTimeoutSeconds*time.Second)
}

// CreateOrAttachVDO idempotently ensures a VDO-backed logical volume exists on top of
// devicePath, named after lvolID, and returns the resulting device path to format/mount.
// If the VG already exists it is reactivated (never recreated); only a genuinely absent VG
// is created fresh. compression and deduplication are set independently at creation time --
// changing them on an already-existing volume is SetVDOFeatures' job, not this function's.
func CreateOrAttachVDO(
	ctx context.Context, devicePath, lvolID string, compression, deduplication bool,
) (string, error) {
	vg := vgName(lvolID)
	devices := []string{devicePath}

	// LVM discovers PVs by content-scanning visible devices, not by remembering a path --
	// refresh its view before checking, since devicePath may have just reappeared under a
	// new device node (reconnect) or be genuinely new. Scoped to devicePath so this never
	// registers the volume's other (redundant HA) local device node into LVM's cache
	// alongside it.
	rescanCtx, cancel := withVDOTimeout(ctx)
	if err := lvmManager.Rescan(rescanCtx, devices); err != nil {
		klog.Warningf("CreateOrAttachVDO: pvscan --cache %s: %v", devicePath, err)
	}
	cancel()

	existsCtx, cancel := withVDOTimeout(ctx)
	currentVG, err := lvmManager.VolumeGroup(existsCtx, devicePath)
	cancel()
	if err != nil {
		return "", fmt.Errorf("probe VG identity of %s: %w", devicePath, err)
	}

	if currentVG == vg {
		hasLVCtx, cancel := withVDOTimeout(ctx)
		hasLV, err := lvmManager.HasLogicalVolume(hasLVCtx, devices, vg, lvolID)
		cancel()
		if err != nil {
			return "", fmt.Errorf("check %s for logical volume %s: %w", vg, lvolID, err)
		}
		if hasLV {
			activateCtx, cancel := withVDOTimeout(ctx)
			err := lvmManager.ActivateVolumeGroup(activateCtx, devices, vg)
			cancel()
			if err != nil {
				return "", fmt.Errorf("reactivate VG %s: %w", vg, err)
			}
			return vdoDevicePath(lvolID), nil
		}
		// Orphaned VG from an earlier interrupted create: pvcreate+vgcreate completed but
		// lvcreate never did (confirmed live -- an earlier attempt hit this exact state
		// with "#LV 0"), so there is nothing here to reactivate. Every subsequent stage
		// attempt would otherwise reactivate an empty VG forever, never producing a
		// mountable device. Remove it and fall through to a fresh create.
		klog.Warningf(
			"CreateOrAttachVDO: VG %s exists but has no %s logical volume -- removing orphaned VG and recreating", //nolint:lll
			vg, lvolID,
		)
		vgremoveCtx, cancel := withVDOTimeout(ctx)
		err = lvmManager.RemoveVolumeGroup(vgremoveCtx, devices, vg)
		cancel()
		if err != nil {
			return "", fmt.Errorf("remove orphaned empty VG %s: %w", vg, err)
		}
	}

	pvcreateCtx, cancel := withVDOTimeout(ctx)
	err = lvmManager.CreatePhysicalVolume(pvcreateCtx, devices, devicePath)
	cancel()
	if err != nil {
		return "", err
	}
	vgcreateCtx, cancel := withVDOTimeout(ctx)
	err = lvmManager.CreateVolumeGroup(vgcreateCtx, devices, vg, devicePath)
	cancel()
	if err != nil {
		return "", err
	}
	lvcreateCtx, cancel := withVDOTimeout(ctx)
	err = lvmManager.CreateVDOLogicalVolume(lvcreateCtx, devices, vg, poolLVName, lvolID, compression, deduplication)
	cancel()
	if err != nil {
		return "", err
	}

	return vdoDevicePath(lvolID), nil
}

// ResolveClonedVDO handles the case where devicePath is a block-level CoW clone/snapshot
// restore of another VDO-formatted volume: a byte-copy carries the source's PV/VG UUIDs and
// VG name verbatim, which CreateOrAttachVDO's own "does vg-<lvolID> exist" check cannot
// detect (the VG on disk is still named after the source). Must run BEFORE
// CreateOrAttachVDO whenever the volume was provisioned from a VolumeContentSource.
func ResolveClonedVDO(ctx context.Context, devicePath, lvolID string) error {
	devices := []string{devicePath}

	rescanCtx, cancel := withVDOTimeout(ctx)
	if err := lvmManager.Rescan(rescanCtx, devices); err != nil {
		klog.Warningf("ResolveClonedVDO: pvscan --cache %s: %v", devicePath, err)
	}
	cancel()

	vg := vgName(lvolID)
	identityCtx, cancel := withVDOTimeout(ctx)
	currentVG, err := lvmManager.VolumeGroup(identityCtx, devicePath)
	cancel()
	if err != nil {
		return fmt.Errorf("probe VG identity of %s: %w", devicePath, err)
	}
	if currentVG == "" || currentVG == vg {
		// Blank device, or already carries this volume's own identity -- nothing to do.
		return nil
	}

	klog.Warningf(
		"device %s for volume %s carries a foreign VG identity %q (byte-level clone) -- regenerating PV/VG UUIDs and renaming to %s", //nolint:lll
		devicePath, lvolID, currentVG, vg,
	)

	importCtx, cancel := withVDOTimeout(ctx)
	err = lvmManager.ImportClonedVolumeGroup(importCtx, devices, vg, devicePath)
	cancel()
	if err != nil {
		return err
	}

	// vgimportclone renames the VG but leaves the VDO logical volume inside named after the
	// source volume. Rename it to lvolID so every subsequent operation (CreateOrAttachVDO's
	// identity check, mount, grow, remove) can address it consistently by this volume's own
	// identity, not the source's.
	listCtx, cancel := withVDOTimeout(ctx)
	names, err := lvmManager.ListLogicalVolumes(listCtx, devices, vg)
	cancel()
	if err != nil {
		return fmt.Errorf("list LVs in %s after clone resolution: %w", vg, err)
	}
	for _, name := range names {
		if name == poolLVName || name == lvolID {
			continue
		}
		renameCtx, cancel := withVDOTimeout(ctx)
		err := lvmManager.RenameLogicalVolume(renameCtx, devices, vg, name, lvolID)
		cancel()
		if err != nil {
			return err
		}
		break
	}
	return nil
}

// DeactivateVDO deactivates (but does not destroy) the VG/LV stack for lvolID. This is the
// correct, non-destructive counterpart to a plain NVMe-oF disconnect: NodeUnstageVolume
// fires any time no pod on this node currently needs the volume mounted (including a
// routine pod delete+recreate on the same node), not only when the volume is actually being
// deleted -- confirmed live: calling RemoveVDO (vgremove) from NodeUnstageVolume destroyed
// a volume's VDO metadata/data on an ordinary pod recreate, well before the volume was
// ever meant to be removed. CreateOrAttachVDO's own vgchange -ay reactivates this exact
// state later without recreating anything.
//
// Unscoped (no device list): by the time this runs the backing device may already be
// unreachable (see the dmsetup fallback below), so it addresses the VG by name alone, the
// same as the original direct-exec implementation did.
func DeactivateVDO(ctx context.Context, lvolID string) error {
	vg := vgName(lvolID)
	deactivateCtx, cancel := withVDOTimeout(ctx)
	err := lvmManager.DeactivateVolumeGroup(deactivateCtx, nil, vg)
	cancel()
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "Failed to find") {
		return fmt.Errorf("deactivate VG %s: %w", vg, err)
	}

	// Confirmed live: this is the same "Volume group ... not found" failure vgremove hits in
	// RemoveVDO, when the backing device disappeared without a clean unstage (storage-side
	// disconnect, force-reschedule) -- vgchange -an needs to read/write the VG's own metadata
	// to deactivate it safely, and there is none left to read. Falling back to a direct
	// dmsetup removal here is safe, not destructive: it only clears this host's own dead
	// references to a device that has already vanished, not the actual VDO metadata/data on
	// the (still-existing, just currently unreachable) backend volume. Without this fallback,
	// NodeUnstageVolume fails indefinitely and the orphaned dm-vdo stack is never cleaned up.
	klog.Warningf(
		"vgchange -an %s failed (backing device unreachable) -- falling back to direct dmsetup removal of any live dm nodes", //nolint:lll
		vg,
	)
	removeCtx, cancel := withVDOTimeout(ctx)
	defer cancel()
	return lvmManager.RemoveOrphanedDMNodes(removeCtx, vg)
}

// RemoveVDO deactivates and removes the VG/LV stack for lvolID, destroying its data. Only
// appropriate when the underlying volume itself is actually being removed, or to clean up
// a stale/orphaned stack whose backing device is already gone -- never call this from a
// routine NodeUnstageVolume. If the backing device is unreachable (crash / force-detach
// without a clean unstage -- see the design doc's orphaned-stack finding), normal vgremove
// fails because it needs to read/write metadata that lives on the now-gone device; falls
// back to direct dmsetup removal of any live dm nodes in that case.
func RemoveVDO(ctx context.Context, lvolID string) error {
	vg := vgName(lvolID)
	deactivateCtx, cancel := withVDOTimeout(ctx)
	_ = lvmManager.DeactivateVolumeGroup(deactivateCtx, nil, vg)
	cancel()

	removeCtx, cancel := withVDOTimeout(ctx)
	err := lvmManager.RemoveVolumeGroup(removeCtx, nil, vg)
	cancel()
	if err == nil {
		return nil
	}

	klog.Warningf(
		"vgremove %s failed (backing device likely unreachable) -- falling back to direct dmsetup removal of any live dm nodes", //nolint:lll
		vg,
	)
	dmCtx, cancel := withVDOTimeout(ctx)
	defer cancel()
	return lvmManager.RemoveOrphanedDMNodes(dmCtx, vg)
}

// GrowVDO extends the VG's pool LV to consume all newly available physical space on
// devicePath (matching the 100%FREE convention used at creation time), then grows the VDO
// logical volume's own size to match, and returns its device path. Must be called with the
// pool LV inactive-safe (it is always active during a live grow -- lvextend supports this
// online).
func GrowVDO(ctx context.Context, devicePath, lvolID string) (string, error) {
	vg := vgName(lvolID)
	devices := []string{devicePath}

	pvresizeCtx, cancel := withVDOTimeout(ctx)
	err := lvmManager.ExpandPhysicalVolume(pvresizeCtx, devices, devicePath)
	cancel()
	if err != nil {
		return "", err
	}
	// lvextend's -l (unlike lvcreate's) treats a bare "100%FREE" as an ABSOLUTE target size
	// (100% of what's currently free), not "grow by" -- confirmed live ("New size given
	// (1024 extents) not larger than existing size (1535 extents)"), since free-space-alone
	// is smaller than the pool's current size. ExpandLogicalVolume's "+" prefix makes it
	// additive (current size + free space), which is what "grow to consume all newly
	// available space" means.
	lvextendPoolCtx, cancel := withVDOGrowTimeout(ctx)
	err = lvmManager.ExpandLogicalVolume(lvextendPoolCtx, devices, vg, poolLVName)
	cancel()
	if err != nil {
		return "", err
	}

	// The VDO logical volume's size is independent of the pool's physical size (it can
	// safely be sized up to the pool's own capacity without relying on dedup/compression
	// savings, matching how creation omits -V to get the same "largest safe size" default).
	// Read the pool's new size back and extend the logical volume to match it.
	sizeCtx, cancel := withVDOTimeout(ctx)
	poolSize, err := lvmManager.LogicalVolumeSize(sizeCtx, devices, vg, poolLVName)
	cancel()
	if err != nil {
		return "", fmt.Errorf("read grown pool size for %s/%s: %w", vg, poolLVName, err)
	}
	lvextendLVCtx, cancel := withVDOGrowTimeout(ctx)
	err = lvmManager.ExtendLogicalVolumeToSize(lvextendLVCtx, devices, vg, lvolID, poolSize)
	cancel()
	if err != nil {
		return "", err
	}
	return vdoDevicePath(lvolID), nil
}

// SetVDOFeatures toggles compression/deduplication on an existing, active VDO volume
// without recreating it. Not wired into any live update path in v1 -- included because the
// underlying mechanism exists and is needed if a future StorageClass/VolumeAttributesClass
// update path is added.
func SetVDOFeatures(ctx context.Context, lvolID string, compression, deduplication bool) error {
	vg := vgName(lvolID)
	lvchangeCtx, cancel := withVDOTimeout(ctx)
	defer cancel()
	return lvmManager.SetVDOFeatures(lvchangeCtx, nil, vg, poolLVName, compression, deduplication)
}
