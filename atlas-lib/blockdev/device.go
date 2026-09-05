// Package blockdev answers two questions about a Linux block device: what it is,
// and what it carries.
//
// The first is Device, a value describing the device as the kernel presents it,
// independent of what produced it. The second is the content reading in
// content.go, which is the evidence an irreversible write rests on: a mkfs or a
// pvcreate may only run against a device positively read as holding nothing.
//
// Both are specified by designs under operator/docs/designs:
// design-node-volume-stack.md Appendix A for Device, and
// design-device-content-detection.md for the reading.
package blockdev

// Device is one Linux block device as the kernel presents it, independent of
// what produced it: an NVMe namespace, a device-mapper node, or a disk handed to
// a storage cluster at deployment.
//
// A path is not an identity and is not sufficient on its own. /dev/dm-3 and
// /dev/mapper/vg--name-lv are one object under two strings whose escaping rules
// differ, and a path does not survive a reconnect, so the major and minor
// numbers travel beside it as the stable identifier a caller can compare on.
//
// The block sizes are here because a caller needs them and deriving them at the
// call site is one more inspection of a path. A mkfs aligns to the logical block
// size, and the content reading resolves its block-relative offsets against it:
// a GPT header is at LBA 1, which is offset 512 on a 512-byte-sector device and
// 4096 on a 4Kn one.
//
// Device is the intersection and not a union. There is no NVMe field, no LVM
// field, and no discriminator saying which produced it, because a layer needing
// NVMe specifics resolves that value from atlas's NVMe package instead.
//
// It is a value rather than a handle, following the immutable-snapshot
// convention the NVMe package holds to: a snapshot is re-resolved rather than
// refreshed in place.
type Device struct {
	// Path is the canonical /dev path.
	Path string

	// Name is the kernel name, such as nvme0n1 or dm-3.
	Name string

	// Major and Minor are the device numbers, which identify the device when a
	// path does not.
	Major, Minor uint32

	// LogicalBlockSize is the unit an offset counted in blocks is resolved
	// against, and the alignment a direct read has to satisfy.
	LogicalBlockSize uint32

	// PhysicalBlockSize is the unit the device prefers writes in.
	PhysicalBlockSize uint32

	// SizeBytes is the device's capacity, and is what locates the tail region a
	// content reading has to look at.
	SizeBytes uint64

	// ReadOnly reports whether the kernel presents the device read-only.
	ReadOnly bool
}
