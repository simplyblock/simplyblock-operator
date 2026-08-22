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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

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

// vgName returns the VG name this volume's VDO stack lives in.
func vgName(lvolID string) string {
	return vgPrefix + lvolID
}

// vdoDevicePath returns the path to the VDO logical volume -- the device to format/mount,
// as opposed to devicePath (the raw NVMe-oF device VDO sits on top of).
func vdoDevicePath(lvolID string) string {
	return fmt.Sprintf("/dev/%s/%s", vgName(lvolID), lvolID)
}

func yn(b bool) string {
	if b {
		return "y"
	}
	return "n"
}

// runLVMCommand runs an LVM/dm-vdo management command with a bounded timeout, logging and
// error-wrapping consistent with initiator.go's execWithTimeout, but also returning output
// since callers here need to parse vgs/lvs/pvs/dmsetup output, not just a success/failure.
func runLVMCommand(ctx context.Context, timeoutSeconds int, args ...string) (string, error) {
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	klog.Infof("running command: %v", args)
	//nolint:gosec // runLVMCommand assumes valid cmd arguments
	cmd := exec.CommandContext(execCtx, args[0], args[1:]...)
	// This container has no udev daemon, so device-mapper's usual wait-for-udev-to-create-
	// the-device-node handshake never completes -- confirmed live ("device not cleared,
	// Aborting. Failed to wipe start of new LV" from lvcreate). DM_DISABLE_UDEV tells
	// device-mapper to create/manage nodes itself instead of waiting on udev, the standard
	// fix for running LVM tools inside a container.
	cmd.Env = append(os.Environ(), "DM_DISABLE_UDEV=1")
	output, err := cmd.CombinedOutput()

	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return string(output), fmt.Errorf("timed out running %v", args)
	}
	if err != nil {
		return string(output), fmt.Errorf("%v: %w: %s", args, err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// devicesArgs scopes an LVM command to exactly one device via LVM's --devices option,
// bypassing its normal system-wide device scan entirely. Required because a volume's two
// redundant NVMe-oF HA paths each surface as their own local device node while presenting
// byte-identical backend data -- confirmed live via "PV /dev/nvme2n1 ... is duplicate for
// PVID ... on /dev/nvme3n1" -- so an unscoped pvscan/pvs/vgs/lvcreate can resolve against
// whichever duplicate LVM's cache happens to pick, non-deterministically, including one that
// was never actually written to by this volume's own CreateOrAttachVDO call.
func devicesArgs(devicePath string) []string {
	return []string{"--devices", devicePath}
}

// vgExists reports whether devicePath's own on-disk PV signature already belongs to vg.
// Content-based (via pvVGName), not a name-based `vgs <name>` lookup -- confirmed live that
// `vgs --devices devicePath vgname` can report success for a VG name that was never actually
// created on that device (this host has an LVM devices file restricting default visibility
// to unrelated devices; querying by name alone, even with --devices, does not reliably tie
// the answer back to devicePath specifically). Content-based matching is what
// ResolveClonedVDO already relies on for the same class of identity question.
func vgExists(ctx context.Context, devicePath, vg string) bool {
	return pvVGName(ctx, devicePath) == vg
}

// pvVGName returns the VG name a device's on-disk PV signature currently belongs to, or ""
// if the device carries no LVM signature at all (a genuinely blank device, or the probe
// itself failing -- both are treated the same way by callers: nothing to resolve). Scoped to
// devicePath -- see devicesArgs.
func pvVGName(ctx context.Context, devicePath string) string {
	args := append([]string{"pvs"}, devicesArgs(devicePath)...)
	args = append(args, "--noheadings", "-o", "vg_name", devicePath)
	out, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, args...)
	if err != nil {
		return ""
	}
	// runLVMCommand merges stdout+stderr, and pvs can print WARNING: lines ahead of the
	// actual field value (e.g. duplicate-PV warnings on a byte-level clone, confirmed
	// live) -- take the first non-empty, non-warning line rather than trusting the whole
	// trimmed blob, which would otherwise pollute both the identity comparison below and
	// any log message built from it.
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "WARNING:") {
			continue
		}
		return line
	}
	return ""
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

	// LVM discovers PVs by content-scanning visible devices, not by remembering a path --
	// refresh its view before checking, since devicePath may have just reappeared under a
	// new device node (reconnect) or be genuinely new. Scoped to devicePath -- see
	// devicesArgs -- so this never registers the volume's other (redundant HA) local device
	// node into LVM's cache alongside it.
	pvscanArgs := append([]string{"pvscan"}, devicesArgs(devicePath)...)
	pvscanArgs = append(pvscanArgs, "--cache", devicePath)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, pvscanArgs...); err != nil {
		klog.Warningf("CreateOrAttachVDO: pvscan --cache %s: %v", devicePath, err)
	}

	if vgExists(ctx, devicePath, vg) {
		vgchangeArgs := append([]string{"vgchange"}, devicesArgs(devicePath)...)
		vgchangeArgs = append(vgchangeArgs, "-ay", vg)
		if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, vgchangeArgs...); err != nil {
			return "", fmt.Errorf("reactivate VG %s: %w", vg, err)
		}
		return vdoDevicePath(lvolID), nil
	}

	pvcreateArgs := append([]string{"pvcreate"}, devicesArgs(devicePath)...)
	pvcreateArgs = append(pvcreateArgs, devicePath)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, pvcreateArgs...); err != nil {
		return "", fmt.Errorf("pvcreate %s: %w", devicePath, err)
	}
	vgcreateArgs := append([]string{"vgcreate"}, devicesArgs(devicePath)...)
	vgcreateArgs = append(vgcreateArgs, vg, devicePath)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, vgcreateArgs...); err != nil {
		return "", fmt.Errorf("vgcreate %s on %s: %w", vg, devicePath, err)
	}
	lvcreateArgs := append([]string{"lvcreate"}, devicesArgs(devicePath)...)
	lvcreateArgs = append(lvcreateArgs,
		"--type", "vdo",
		"--config", "activation{checks=0}",
		"-n", lvolID,
		"-l", "100%FREE",
		"--compression", yn(compression),
		"--deduplication", yn(deduplication),
		vg+"/"+poolLVName,
		"--yes",
	)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, lvcreateArgs...); err != nil {
		return "", fmt.Errorf("lvcreate --type vdo for %s: %w", vg, err)
	}

	return vdoDevicePath(lvolID), nil
}

// ResolveClonedVDO handles the case where devicePath is a block-level CoW clone/snapshot
// restore of another VDO-formatted volume: a byte-copy carries the source's PV/VG UUIDs and
// VG name verbatim, which CreateOrAttachVDO's own "does vg-<lvolID> exist" check cannot
// detect (the VG on disk is still named after the source). Must run BEFORE
// CreateOrAttachVDO whenever the volume was provisioned from a VolumeContentSource.
func ResolveClonedVDO(ctx context.Context, devicePath, lvolID string) error {
	pvscanArgs := append([]string{"pvscan"}, devicesArgs(devicePath)...)
	pvscanArgs = append(pvscanArgs, "--cache", devicePath)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, pvscanArgs...); err != nil {
		klog.Warningf("ResolveClonedVDO: pvscan --cache %s: %v", devicePath, err)
	}

	vg := vgName(lvolID)
	currentVG := pvVGName(ctx, devicePath)
	if currentVG == "" || currentVG == vg {
		// Blank device, or already carries this volume's own identity -- nothing to do.
		return nil
	}

	klog.Warningf(
		"device %s for volume %s carries a foreign VG identity %q (byte-level clone) -- regenerating PV/VG UUIDs and renaming to %s", //nolint:lll
		devicePath, lvolID, currentVG, vg,
	)

	vgimportcloneArgs := append([]string{"vgimportclone"}, devicesArgs(devicePath)...)
	vgimportcloneArgs = append(vgimportcloneArgs, "--basevgname", vg, devicePath)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, vgimportcloneArgs...); err != nil {
		return fmt.Errorf("vgimportclone %s to %s: %w", devicePath, vg, err)
	}

	// vgimportclone renames the VG but leaves the VDO logical volume inside named after the
	// source volume. Rename it to lvolID so every subsequent operation (CreateOrAttachVDO's
	// vgs/lvs check, mount, grow, remove) can address it consistently by this volume's own
	// identity, not the source's.
	lvsArgs := append([]string{"lvs"}, devicesArgs(devicePath)...)
	lvsArgs = append(lvsArgs, "--noheadings", "-o", "lv_name", vg)
	out, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, lvsArgs...)
	if err != nil {
		return fmt.Errorf("list LVs in %s after clone resolution: %w", vg, err)
	}
	for name := range strings.FieldsSeq(out) {
		if name == poolLVName || name == lvolID {
			continue
		}
		lvrenameArgs := append([]string{"lvrename"}, devicesArgs(devicePath)...)
		lvrenameArgs = append(lvrenameArgs, vg, name, lvolID)
		if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, lvrenameArgs...); err != nil {
			return fmt.Errorf("rename cloned LV %s/%s to %s: %w", vg, name, lvolID, err)
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
func DeactivateVDO(ctx context.Context, lvolID string) error {
	vg := vgName(lvolID)
	_, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, "vgchange", "-an", vg)
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
	return removeOrphanedDMNodes(ctx, vg)
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
	_, _ = runLVMCommand(ctx, vdoCmdTimeoutSeconds, "vgchange", "-an", vg)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, "vgremove", "-f", vg); err == nil {
		return nil
	}

	klog.Warningf(
		"vgremove %s failed (backing device likely unreachable) -- falling back to direct dmsetup removal of any live dm nodes", //nolint:lll
		vg,
	)
	return removeOrphanedDMNodes(ctx, vg)
}

// removeOrphanedDMNodes clears any live device-mapper nodes for vg directly, for the case
// where the backing device is gone and vgremove can no longer read the metadata it needs.
// The VDO target depends on its backing dm mapping, so removal order matters, but the exact
// dependency chain isn't worth hardcoding -- retry across a few passes so removing a
// dependent unblocks what it was blocking, mirroring how this was resolved by hand during
// the design's own spike investigation.
func removeOrphanedDMNodes(ctx context.Context, vg string) error {
	out, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, "dmsetup", "ls")
	if err != nil {
		return fmt.Errorf("dmsetup ls: %w", err)
	}

	// device-mapper flattens "<vg>-<lv>" into a single dm device name by doubling every
	// literal "-" within the VG/LV name components and using a single "-" as the separator
	// -- confirmed live: matching against the unescaped VG name found nothing, leaving the
	// stack orphaned even though this exact fallback had just been chosen specifically to
	// clean it up.
	escapedVG := strings.ReplaceAll(vg, "-", "--")

	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "No devices found" {
			continue
		}
		name := strings.Fields(line)[0]
		if strings.HasPrefix(name, escapedVG+"-") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}

	var lastErr error
	for pass := 0; pass < 3 && len(names) > 0; pass++ {
		var remaining []string
		for _, name := range names {
			if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, "dmsetup", "remove", name); err != nil {
				remaining = append(remaining, name)
				lastErr = err
			}
		}
		names = remaining
	}
	if len(names) > 0 {
		return fmt.Errorf("failed to remove orphaned dm nodes %v: %w", names, lastErr)
	}
	return nil
}

// GrowVDO extends the VG's pool LV to consume all newly-available physical space on
// devicePath (matching the 100%FREE convention used at creation time), then grows the VDO
// logical volume's own size to match, and returns its device path. Must be called with the
// pool LV inactive-safe (it is always active during a live grow -- lvextend supports this
// online).
func GrowVDO(ctx context.Context, devicePath, lvolID string) (string, error) {
	vg := vgName(lvolID)

	pvresizeArgs := append([]string{"pvresize"}, devicesArgs(devicePath)...)
	pvresizeArgs = append(pvresizeArgs, devicePath)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, pvresizeArgs...); err != nil {
		return "", fmt.Errorf("pvresize %s: %w", devicePath, err)
	}
	// lvextend's -l (unlike lvcreate's) treats a bare "100%FREE" as an ABSOLUTE target size
	// (100% of what's currently free), not "grow by" -- confirmed live ("New size given
	// (1024 extents) not larger than existing size (1535 extents)"), since free-space-alone
	// is smaller than the pool's current size. The "+" prefix makes it additive (current
	// size + free space), which is what "grow to consume all newly-available space" means.
	lvextendPoolArgs := append([]string{"lvextend"}, devicesArgs(devicePath)...)
	lvextendPoolArgs = append(lvextendPoolArgs, "-l+100%FREE", vg+"/"+poolLVName)
	if _, err := runLVMCommand(ctx, vdoGrowTimeoutSeconds, lvextendPoolArgs...); err != nil {
		return "", fmt.Errorf("grow VDO pool LV %s/%s: %w", vg, poolLVName, err)
	}

	// The VDO logical volume's size is independent of the pool's physical size (it can
	// safely be sized up to the pool's own capacity without relying on dedup/compression
	// savings, matching how creation omits -V to get the same "largest safe size" default).
	// Read the pool's new size back and extend the logical volume to match it.
	lvsArgs := append([]string{"lvs"}, devicesArgs(devicePath)...)
	lvsArgs = append(lvsArgs, "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", vg+"/"+poolLVName)
	poolSize, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds, lvsArgs...)
	if err != nil {
		return "", fmt.Errorf("read grown pool size for %s/%s: %w", vg, poolLVName, err)
	}
	newSize := strings.TrimSpace(poolSize)
	lvextendLVArgs := append([]string{"lvextend"}, devicesArgs(devicePath)...)
	lvextendLVArgs = append(lvextendLVArgs, "-L"+newSize+"B", vg+"/"+lvolID)
	if _, err := runLVMCommand(ctx, vdoGrowTimeoutSeconds, lvextendLVArgs...); err != nil {
		return "", fmt.Errorf("grow VDO logical volume %s/%s to %sB: %w", vg, lvolID, newSize, err)
	}
	return vdoDevicePath(lvolID), nil
}

// SetVDOFeatures toggles compression/deduplication on an existing, active VDO volume
// without recreating it. Not wired into any live update path in v1 -- included because the
// underlying mechanism exists and is needed if a future StorageClass/VolumeAttributesClass
// update path is added.
func SetVDOFeatures(ctx context.Context, lvolID string, compression, deduplication bool) error {
	vg := vgName(lvolID)
	if _, err := runLVMCommand(ctx, vdoCmdTimeoutSeconds,
		"lvchange", "--compression", yn(compression), "--deduplication", yn(deduplication), vg+"/"+poolLVName,
	); err != nil {
		return fmt.Errorf("set VDO features on %s/%s: %w", vg, poolLVName, err)
	}
	return nil
}
