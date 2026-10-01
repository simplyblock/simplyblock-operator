// Naming the namespace behind a path on this host.
//
// The lookups beside this one take an identity the caller already has: a UUID,
// a subsystem and namespace id, a device path. This one starts from a path that
// names none of them — a staging directory, or the device file a raw volume was
// published as — and asks the kernel what is under it.
//
// It matches on the device number rather than on the path, because a device has
// as many names as udev gave it. A namespace connected by this driver is
// /dev/nvme0n1, /dev/disk/by-id/nvme-uuid.<uuid>, /dev/disk/by-id/nvme-<model>_
// <serial>_<nsid>, and any by-path link besides, and a caller holding one of
// those has no way to know which. All of them stat to the one number sysfs
// records in the namespace's `dev`, which is why that is what is compared.

package nvme

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/simplyblock/atlas/errs"
)

// DeviceNumberAt is the device number a path resolves to, as `major:minor`.
//
// For a block device file it is the device the file represents, and for
// anything else it is the device holding the filesystem the path is on. That
// covers the two shapes a staged volume takes: a directory the filesystem is
// mounted at, and the device file a raw block volume is published as.
func DeviceNumberAt(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("nvme: stat %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("nvme: %s reports no device number on this platform", path)
	}

	number := uint64(stat.Dev) //nolint:unconvert,gosec // Dev is int32 on darwin
	if info.Mode()&os.ModeDevice != 0 {
		number = uint64(stat.Rdev) //nolint:unconvert,gosec // as above
	}
	return fmt.Sprintf("%d:%d", unix.Major(number), unix.Minor(number)), nil
}

// ByDeviceNumber returns the namespace the kernel knows by this `major:minor`.
func (r *SysfsDeviceResolver) ByDeviceNumber(ctx context.Context, number string) (Device, error) {
	devices, err := r.List(ctx)
	if err != nil {
		return Device{}, err
	}
	for _, device := range devices {
		if device.Namespace.Dev == number {
			return device, nil
		}
	}
	return Device{}, fmt.Errorf("device number %s: %w", number, errs.ErrNotFound)
}
