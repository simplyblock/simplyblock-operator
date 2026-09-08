// The block-device half of a namespace, named on its own.
//
// A Namespace already carries the name, the device path, the device numbers, the
// logical block size, the capacity, and the read-only flag. Those are facts
// about a block device rather than facts about NVMe, and a consumer that needs
// them needs them the same way for a device-mapper node or a disk handed to a
// storage cluster. blockdev.Device is that half, and this is where a namespace
// answers with it instead of a caller assembling one from six fields and
// getting the units wrong.

package nvme

import (
	"strconv"
	"strings"

	"github.com/simplyblock/atlas/blockdev"
)

// BlockDevice is the namespace as a plain block device, for a caller that has no
// business knowing it is NVMe: the content probe, and anything else that reads
// or measures a device rather than addressing a fabric.
//
// The size conversion is the reason this is a method rather than a struct
// literal at each call site. Capacity is in 512-byte sectors whatever the
// namespace's logical block size is, so a caller multiplying it by
// LogicalBlockSize gets an answer that is right for the common case and eight
// times too large for a 4Kn namespace.
func (n Namespace) BlockDevice() blockdev.Device {
	major, minor := parseDevNumbers(n.Dev)
	return blockdev.Device{
		Path:             n.DevicePath,
		Name:             n.Name,
		Major:            major,
		Minor:            minor,
		LogicalBlockSize: n.LogicalBlockSize,
		// A namespace exposes no physical block size of its own, and reporting
		// the logical one is what the kernel does for a device that does not
		// distinguish them.
		PhysicalBlockSize: n.LogicalBlockSize,
		SizeBytes:         n.Capacity * 512,
		ReadOnly:          n.ReadOnly,
	}
}

// parseDevNumbers splits the sysfs `dev` attribute, which is `major:minor`.
// A value that is not that shape yields zeros, which is what a caller comparing
// on device numbers has to treat as "unknown" anyway.
func parseDevNumbers(dev string) (major, minor uint32) {
	majorText, minorText, ok := strings.Cut(dev, ":")
	if !ok {
		return 0, 0
	}
	m, err := strconv.ParseUint(majorText, 10, 32)
	if err != nil {
		return 0, 0
	}
	n, err := strconv.ParseUint(minorText, 10, 32)
	if err != nil {
		return 0, 0
	}
	return uint32(m), uint32(n)
}
