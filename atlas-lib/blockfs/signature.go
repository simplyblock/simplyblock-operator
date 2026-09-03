// The on-disk signatures this package recognizes, and the match against a
// device's leading bytes. They live here, apart from the probe itself, because
// the table is the part that grows: every entry is a magic number at a fixed
// offset, and adding one is data rather than logic.
package blockfs

import "bytes"

// probeLength is how much of a device's start is read. It is set by the
// furthest signature below — Btrfs, whose superblock sits at 64 KiB — rounded
// up to a power of two, so every offset in the table falls inside one read.
const probeLength = 128 << 10

// signature is a magic number at a fixed offset from the start of a device.
type signature struct {
	// name is what gets reported and logged: a filesystem family, or the name
	// of whatever else claimed the device.
	name string
	// offset is where magic begins, in bytes from the start of the device.
	offset int
	magic  []byte
	// mountable distinguishes a filesystem, which a consumer can mount and use
	// as it is, from a signature that merely proves the device is somebody's:
	// an encrypted container, an LVM physical volume, a swap area, or a
	// partition table.
	mountable bool
}

// signatures are matched in order, so the entries that identify a device
// beyond doubt come before the weaker ones. A GPT disk carries a protective
// MBR, so GPT precedes it, and the two-byte MBR boot signature — the one entry
// short enough to turn up in unrelated data — comes last.
//
// The ext entry covers ext2, ext3, and ext4 alike: they share s_magic, and
// telling them apart means reading feature flags that no caller here needs,
// since the only decision at stake is whether the device may be formatted.
var signatures = []signature{
	{name: "xfs", offset: 0, magic: []byte("XFSB"), mountable: true},
	{name: "ext", offset: 1024 + 0x38, magic: []byte{0x53, 0xEF}, mountable: true},
	{name: "btrfs", offset: 0x10040, magic: []byte("_BHRfS_M"), mountable: true},
	{name: "LUKS", offset: 0, magic: []byte{'L', 'U', 'K', 'S', 0xBA, 0xBE}},
	// An LVM physical volume's label may sit in any of the first four sectors.
	{name: "LVM2", offset: 0, magic: []byte("LABELONE")},
	{name: "LVM2", offset: 512, magic: []byte("LABELONE")},
	{name: "LVM2", offset: 1024, magic: []byte("LABELONE")},
	{name: "LVM2", offset: 1536, magic: []byte("LABELONE")},
	{name: "swap", offset: 4096 - 10, magic: []byte("SWAPSPACE2")},
	{name: "GPT", offset: 512, magic: []byte("EFI PART")},
	{name: "MBR", offset: 510, magic: []byte{0x55, 0xAA}},
}

// match returns the first signature present in data. A signature whose offset
// lies beyond the bytes that were read is skipped rather than treated as
// absent: a device smaller than one signature's offset simply cannot carry it.
func match(data []byte) (signature, bool) {
	for _, s := range signatures {
		end := s.offset + len(s.magic)
		if end > len(data) {
			continue
		}
		if bytes.Equal(data[s.offset:end], s.magic) {
			return s, true
		}
	}
	return signature{}, false
}

// isZero reports whether every byte in data is zero.
func isZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}
