// Package mount reads what a block device carries, puts a filesystem on it
// when it carries nothing, and owns the lifecycle of the directory or file it
// gets mounted on.
//
// It is the node-local half of staging a volume, separated from the CSI node
// service because none of it needs a CSI request to answer: whether a device is
// blank, which options mkfs takes for a filesystem, whether a mount has gone
// dead, and whether a path is safe to mount over are all questions about the
// node. The decisions layered on top of those answers, which filesystem a
// volume is supposed to carry and what to do when the device disagrees, stay
// in the node service, where the claim and the volume capability are.
package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
	"k8s.io/klog"
	k8smount "k8s.io/mount-utils"
	"k8s.io/utils/exec"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/errs/deferrers"
)

// Mounter performs the node-local half of staging: reading a device, mounting
// it, and managing the path it is mounted on.
//
// It holds the two seams the operations need, the mount interface and the
// command runner, so a test can drive the whole package without a kernel.
type Mounter struct {
	mounter k8smount.Interface
	execer  exec.Interface
}

// New returns a Mounter driving the real system.
func New() *Mounter {
	return &Mounter{mounter: k8smount.New(""), execer: exec.New()}
}

// NewWith returns a Mounter driving the given seams, for tests.
func NewWith(mounter k8smount.Interface, execer exec.Interface) *Mounter {
	return &Mounter{mounter: mounter, execer: execer}
}

// Mount attaches devicePath at target as fsType, without formatting anything.
// It is the path taken when the device is already known to carry a filesystem.
func (m *Mounter) Mount(devicePath, target, fsType string, flags []string) error {
	return m.mounter.Mount(devicePath, target, fsType, flags)
}

// FormatAndMount attaches devicePath at target, putting fsType on it first if
// it reads blank. The caller is responsible for having established that
// formatting is the right thing to do.
func (m *Mounter) FormatAndMount(devicePath, target, fsType string, flags, formatOptions []string) error {
	safe := k8smount.SafeFormatAndMount{Interface: m.mounter, Exec: m.execer}
	return safe.FormatAndMountSensitiveWithFormatOptions(devicePath, target, fsType, flags, nil, formatOptions)
}

// Unmount detaches whatever is mounted at target.
func (m *Mounter) Unmount(target string) error {
	return m.mounter.Unmount(target)
}

// NeedsResize reports whether the filesystem on devicePath is smaller than the
// device now is.
func (m *Mounter) NeedsResize(devicePath, mountPath string) (bool, error) {
	return k8smount.NewResizeFs(m.execer).NeedResize(devicePath, mountPath)
}

// Resize grows the filesystem on devicePath to fill the device, reporting
// whether it needed resizing.
func (m *Mounter) Resize(devicePath, mountPath string) (bool, error) {
	return k8smount.NewResizeFs(m.execer).Resize(devicePath, mountPath)
}

// Supported reports whether this driver formats and mounts fsType.
func Supported(fsType string) bool {
	return supportedOnDiskFilesystems[fsType]
}

// FlagsFor returns the mount options a filesystem needs regardless of what the
// volume asked for.
func FlagsFor(fsType string) []string {
	if fsType == "xfs" {
		// XFS refuses to mount two filesystems with the same UUID, and nouuid lets a
		// volume and its clone or restored snapshot mount on the same node.
		return []string{"nouuid"}
	}
	return nil
}

// FormatOptions returns the mkfs options for fsType, given the provisioning
// parameters carried in the volume context.
func FormatOptions(fsType string, volumeContext map[string]string) []string {
	if fsType != "xfs" {
		return nil
	}
	options := append([]string{}, xfsFeatureOptions()...)
	return append(options, xfsStripeOptions(volumeContext)...)
}

// ApplyExt4Reserved sets the reserved-block percentage on an ext4 filesystem.
// An empty reserved is not an error: it means the class did not ask for one.
func ApplyExt4Reserved(devicePath, reserved string) error {
	if reserved == "" {
		klog.Infof("No tune2fs_reserved_blocks set; skipping tune2fs adjustment")
		return nil
	}
	output, err := osexec.Command("tune2fs", "-m", reserved, devicePath).CombinedOutput()
	if err != nil {
		klog.Errorf("Failed to apply tune2fs -m %s on %s: %v\nOutput: %s", reserved, devicePath, err, string(output))
		return fmt.Errorf("tune2fs failed: %w", err)
	}
	klog.Infof("Applied tune2fs -m %s on %s", reserved, devicePath)
	return nil
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
// be set together. If only one is set, the defaults are used instead.
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
// new XFS volumes are created with. xfsprogs ships it, and the container image only has
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
// Unlike the ext4 equivalent there is no repair path: XFS features can only be added,
// never removed, and such a filesystem cannot be mounted even read-only. Prevention
// is the only option.
//
// lts_5.15.conf is chosen over the closer lts_6.x baselines because it sits below the
// floor of every el9 minor instead of tracking vendor backports: 9.5 accepts 0xb
// while 9.8 additionally accepts EXCHRANGE and NREXT64. The options compose with
// xfsStripeOptions: stripe geometry lives in sb_unit, sb_width, and sb_logsunit,
// and is
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

// probeDiskFormat reads the filesystem on devicePath through atlas's blockdev
// prober, translating its refusals into this driver's staging language: a probe
// that could not read the device and a partition table where a filesystem was
// expected both leave open whether the device holds somebody's data, so staging
// fails there instead of formatting through the doubt. The next attempt probes
// the device again.
func (m *Mounter) Probe(ctx context.Context, devicePath string) (string, error) {
	fs, err := blockdev.NewBlkidProberWithRunner(execRunner(m.execer)).Format(ctx, devicePath)
	switch {
	case errors.Is(err, blockdev.ErrPartitionTable):
		// Wrapped, because the prober names the table blkid reported and which
		// one it is decides what to do about it: a GPT disk handed to the driver
		// by mistake is a different problem from a stale DOS label on a volume
		// that was reused.
		return "", fmt.Errorf(
			"refusing to stage %s, which carries a partition table rather than a filesystem: %w",
			devicePath, err,
		)
	case err != nil:
		return "", fmt.Errorf(
			"cannot read the on-disk filesystem of %s, refusing to stage a device whose contents are unknown: %w",
			devicePath, err,
		)
	}
	return fs, nil
}

// execRunner adapts the node server's command runner to blockdev's Runner,
// keeping blkid's exit code separate from a transport failure the way the
// prober's contract requires. It runs without the context on purpose: the
// underlying exec.Interface command carries no context, which preserves the
// staging path's existing timeout behavior (none) rather than changing it here.
func execRunner(execer exec.Interface) blockdev.Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, int, error) {
		out, err := execer.Command(name, args...).CombinedOutput()
		if err != nil {
			var exit exec.ExitError
			if errors.As(err, &exit) {
				return out, exit.ExitStatus(), nil
			}
			return out, 0, err
		}
		return out, 0, nil
	}
}

// supportedOnDiskFilesystems are the filesystems this driver formats and mounts.
// The annotation is writable by anyone who can edit the claim, so a value
// outside this set is ignored rather than passed on to mkfs.
var supportedOnDiskFilesystems = map[string]bool{
	"ext4": true,
	"xfs":  true,
}

// stagingMountDead reports whether stagingPath is a dead or corrupted mount: the
// state left behind when total NVMe-oF path loss makes the kernel remove the
// backing device. Such a mount returns ENOTCONN/ESTALE/EIO on access, which
// mount.IsCorruptedMnt detects.
func (m *Mounter) IsDead(stagingPath string) bool {
	if _, err := m.mounter.IsMountPoint(stagingPath); err != nil {
		return k8smount.IsCorruptedMnt(err)
	}
	// IsMountPoint can still succeed on a mount whose device just vanished, so
	// a stat of the path then fails with an EIO-class error.
	fi, err := os.Stat(stagingPath)
	if err != nil {
		return k8smount.IsCorruptedMnt(err)
	}
	// Some filesystems (notably ext4) do NOT shut down when their backing block
	// device is removed on total NVMe-oF path loss, unlike XFS, which goes EIO
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
func (m *Mounter) ForceUnmount(stagingPath string) error {
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

// isStaged if stagingPath is a mount point, it means it is already staged, and vice versa
func (m *Mounter) IsMounted(stagingPath string) (bool, error) {
	isMount, err := m.mounter.IsMountPoint(stagingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		} else if k8smount.IsCorruptedMnt(err) {
			return true, nil
		}
		klog.Warningf("check is stage error: %v", err)
		return false, err
	}
	return isMount, nil
}

// create mount point if not exists, return whether already mounted
func (m *Mounter) EnsureDirectory(path string) (bool, error) {
	isMount, err := m.mounter.IsMountPoint(path)
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
func (m *Mounter) Remove(path string) error {
	isMount, err := m.mounter.IsMountPoint(path)
	if err != nil {
		if os.IsNotExist(err) {
			klog.Infof("%s already deleted", path)
			return nil
		} else if k8smount.IsCorruptedMnt(err) {
			klog.Warningf("Corrupted mount point detected at %s", path)
			isMount = true
		} else {
			klog.Errorf("Error checking mount point %s: %v", path, err)
			return err
		}
	}

	if isMount {
		err = m.mounter.Unmount(path)
		if err != nil {
			return err
		}
	}
	return os.RemoveAll(path)
}

func (m *Mounter) EnsureFile(path string) error {
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
func (m *Mounter) EnsureCleanTarget(targetPath string) error {
	isMount, err := m.mounter.IsMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if k8smount.IsCorruptedMnt(err) {
			isMount = true
		} else {
			return err
		}
	}

	if isMount {
		if err := m.mounter.Unmount(targetPath); err != nil {
			_ = osexec.Command("umount", "-l", targetPath).Run()
		}
	}

	if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func BlockSizeBytes(volumePath string) (uint64, error) {
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
