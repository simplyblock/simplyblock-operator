// What a resolved sysfs device path says about the device at the end of it.
//
// Every class directory under sysfs — class/net, class/block, class/nvme — is a
// directory of symlinks into one shared device tree, and the questions "is this
// backed by hardware" and "which slot is it in" are answered the same way for
// all of them: resolve the link and read the path. Two packages ask, so the
// answer is here rather than copied into both.
//
// The input is always an already-resolved path. Resolving is the caller's step,
// because a caller that has resolved a path once has other uses for it.

package sysfs

import (
	"path/filepath"
	"regexp"
	"strings"
)

// pciAddress matches a PCI address in the domain:bus:device.function form the
// kernel names its device directories with, which is also the form a
// deployment config names a device by.
var pciAddress = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

// virtualSegment is the part of the device tree the kernel puts everything with
// no hardware behind it under: bridges, veth pairs, bonds, VLANs, loopback,
// device-mapper nodes, software RAID, loop devices, and zram.
var virtualSegment = string(filepath.Separator) + filepath.Join("devices", "virtual") +
	string(filepath.Separator)

// PCIAddressOf returns the PCI address of the closest enclosing PCI device on a
// resolved sysfs path, and the empty string when nothing on it is one.
//
// It walks outward from the device rather than inward from the root, because a
// path can cross more than one PCI address — a function behind a bridge behind a
// root port — and the one that identifies the device is the innermost.
func PCIAddressOf(resolved string) string {
	for dir := filepath.Clean(resolved); ; {
		if pciAddress.MatchString(filepath.Base(dir)) {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// IsVirtual reports whether a resolved sysfs path names a device with no
// hardware behind it.
func IsVirtual(resolved string) bool {
	return strings.Contains(filepath.Clean(resolved)+string(filepath.Separator), virtualSegment)
}
