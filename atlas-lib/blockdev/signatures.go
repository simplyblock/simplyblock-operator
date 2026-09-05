// The on-disk signatures a reading recognizes, and where each one lives.
//
// The catalog decides how well a refusal is worded rather than whether it
// happens: a format nobody listed here still writes bytes into a probed region,
// so it fails the zero test and is refused as foreign. That is what keeps an
// incomplete catalog from being a safety problem.
//
// Offsets counted in logical blocks are resolved against the device rather than
// against 512, because a GPT header is at LBA 1, which is offset 4096 on a 4Kn
// namespace. Everything else is an absolute byte offset the format defines.

package blockdev

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// find is one signature located in a probed region.
type find struct {
	content Content
	typ     string
	offset  int64
	detail  string
}

// regions is what was read off a device, and the arithmetic for asking what is
// at an absolute offset. A device smaller than two regions is read whole, and
// then head alone covers it.
type regions struct {
	head   []byte
	tail   []byte
	tailAt int64
	size   int64
	lbs    int64
}

// at returns the n bytes at absolute offset off, and reports whether they were
// among the bytes actually read. An offset between the two regions was not read
// and is not guessed at.
func (r regions) at(off, n int64) ([]byte, bool) {
	if off < 0 || n <= 0 {
		return nil, false
	}
	if off+n <= int64(len(r.head)) {
		return r.head[off : off+n], true
	}
	if len(r.tail) > 0 && off >= r.tailAt && off+n <= r.tailAt+int64(len(r.tail)) {
		s := off - r.tailAt
		return r.tail[s : s+n], true
	}
	return nil, false
}

// eq reports whether the bytes at off equal want.
func (r regions) eq(off int64, want []byte) bool {
	got, ok := r.at(off, int64(len(want)))
	return ok && bytes.Equal(got, want)
}

// u16 and u32 read a little-endian integer, reporting whether it was read at all.
func (r regions) u16(off int64) (uint16, bool) {
	b, ok := r.at(off, 2)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b), true
}

func (r regions) u32(off int64) (uint32, bool) {
	b, ok := r.at(off, 4)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b), true
}

// detect collects every signature the regions carry. Every detector runs: a
// refusal names everything found rather than the first thing found, so an
// operator sees the whole picture in one message.
func detect(r regions) []find {
	var finds []find
	for _, d := range detectors {
		if f, ok := d(r); ok {
			finds = append(finds, f)
		}
	}
	return finds
}

// detectors is the catalog, in no significant order: which find wins is decided
// by choose, not by position here.
var detectors = []func(regions) (find, bool){
	detectExt, detectXFS, detectLVM2, detectLUKS, detectGPT, detectMBR,
	detectExFAT, detectFAT, detectBtrfs, detectSwap, detectMDRaid, detectZFS,
}

// The ext superblock sits at 1024, so its magic is at 1080 and its three
// feature words follow at 1116, 1120, and 1124.
const (
	extSuperblock  = 1024
	extMagicOffset = extSuperblock + 56
	extMagic       = 0xEF53

	extCompatHasJournal = 0x0004

	// The incompatible and read-only bits that only ext4 sets.
	// Anything carrying one of these is ext4 whatever else it says.
	ext4Incompat  = 0x0040 | 0x0080 | 0x0100 | 0x0200 // extents, 64bit, mmp, flex_bg
	ext4RoCompat  = 0x0008 | 0x0010 | 0x0020 | 0x0040 // huge_file, gdt_csum, dir_nlink, extra_isize
	ext4RoCompat2 = 0x0100 | 0x0200 | 0x0400          // quota, bigalloc, metadata_csum
)

// detectExt names the exact member of the ext family. The distinction is worth
// making because a reading that rounded every ext filesystem up to ext4 would
// disagree with the claim annotation for a volume this driver did not create,
// and all three mount through the kernel's ext4 driver anyway.
func detectExt(r regions) (find, bool) {
	if m, ok := r.u16(extMagicOffset); !ok || m != extMagic {
		return find{}, false
	}
	compat, _ := r.u32(extSuperblock + 92)
	incompat, _ := r.u32(extSuperblock + 96)
	roCompat, _ := r.u32(extSuperblock + 100)

	typ := "ext2"
	switch {
	case incompat&ext4Incompat != 0 || roCompat&(ext4RoCompat|ext4RoCompat2) != 0:
		typ = "ext4"
	case compat&extCompatHasJournal != 0:
		typ = "ext3"
	}
	return find{ContentFilesystem, typ, extMagicOffset,
		fmt.Sprintf("%s superblock at %d", typ, extSuperblock)}, true
}

// detectXFS matches the XFS superblock, which starts at offset 0.
func detectXFS(r regions) (find, bool) {
	if !r.eq(0, []byte("XFSB")) {
		return find{}, false
	}
	return find{ContentFilesystem, "xfs", 0, "XFS superblock at 0"}, true
}

// detectLVM2 matches a physical-volume label, the one reading that is a stack
// layer rather than a filesystem.
//
// LVM scans the first four 512-byte units whatever the device's logical block
// size is, so this offset is counted in 512-byte units by the format's own
// definition rather than by the device's geometry.
func detectLVM2(r regions) (find, bool) {
	for i := int64(0); i < 4; i++ {
		off := i * 512
		if !r.eq(off, []byte("LABELONE")) {
			continue
		}
		// The label header is id[8], sector[8], crc[4], offset[4], then the
		// type, so a type of "LVM2 001" sits 24 bytes in.
		if t, ok := r.at(off+24, 8); ok && bytes.HasPrefix(t, []byte("LVM2 ")) {
			return find{ContentStackLayer, "LVM2_member", off,
				fmt.Sprintf("LVM2 physical-volume label at %d", off)}, true
		}
	}
	return find{}, false
}

// detectLUKS matches both LUKS1 and LUKS2, which share a magic at offset 0 and
// differ in the version that follows it.
func detectLUKS(r regions) (find, bool) {
	if !r.eq(0, []byte{'L', 'U', 'K', 'S', 0xba, 0xbe}) {
		return find{}, false
	}
	detail := "LUKS header at 0"
	if b, ok := r.at(6, 2); ok {
		detail = fmt.Sprintf("LUKS%d header at 0", binary.BigEndian.Uint16(b))
	}
	return find{ContentForeign, "crypto_LUKS", 0, detail}, true
}

// detectGPT matches the GPT header signature, which is the Signature field of
// the GPT header structure rather than anything to do with an EFI system
// partition: every GPT carries it. The primary header is at LBA 1 and the
// backup at the last LBA, and either one alone is enough to refuse the device.
func detectGPT(r regions) (find, bool) {
	sig := []byte("EFI PART")
	primary := r.lbs
	if r.eq(primary, sig) {
		return find{ContentForeign, "gpt", primary,
			fmt.Sprintf("GPT header at %d (LBA 1)", primary)}, true
	}
	if backup := r.size - r.lbs; backup > 0 && r.eq(backup, sig) {
		return find{ContentForeign, "gpt", backup,
			fmt.Sprintf("backup GPT header at %d (the last LBA)", backup)}, true
	}
	return find{}, false
}

// detectMBR matches a partition table with at least one used entry. An empty
// table behind the boot signature is not a partition table, and a FAT boot
// sector carries the same signature, which is why choose prefers a more
// specific find at a lower offset.
func detectMBR(r regions) (find, bool) {
	if !r.eq(510, []byte{0x55, 0xAA}) {
		return find{}, false
	}
	table, ok := r.at(446, 64)
	if !ok {
		return find{}, false
	}
	for i := range 4 {
		if table[i*16+4] != 0 { // the partition type byte
			return find{ContentForeign, "dos", 446,
				"MBR partition table at 446, with at least one used entry"}, true
		}
	}
	return find{}, false
}

// FAT carries no single magic, so the BIOS parameter block is validated
// alongside the type string. A boot sector that fails these checks is not
// reported as FAT, and is refused anyway by whatever else the region holds.
func validFATBPB(r regions) bool {
	bytesPerSector, ok := r.u16(0x0B)
	if !ok {
		return false
	}
	switch bytesPerSector {
	case 512, 1024, 2048, 4096:
	default:
		return false
	}
	spc, ok := r.at(0x0D, 1)
	if !ok || spc[0] == 0 || spc[0]&(spc[0]-1) != 0 || spc[0] > 128 {
		return false
	}
	if reserved, ok := r.u16(0x0E); !ok || reserved == 0 {
		return false
	}
	fats, ok := r.at(0x10, 1)
	if !ok || fats[0] < 1 || fats[0] > 2 {
		return false
	}
	media, ok := r.at(0x15, 1)
	if !ok || (media[0] != 0xF0 && media[0] < 0xF8) {
		return false
	}
	return true
}

// detectFAT matches FAT12, FAT16, and FAT32 by their type string, which sits at
// a different offset for FAT32 because its BPB is longer.
func detectFAT(r regions) (find, bool) {
	if !validFATBPB(r) {
		return find{}, false
	}
	for _, c := range []struct {
		off  int64
		want string
		name string
	}{
		{0x52, "FAT32", "FAT32"},
		{0x36, "FAT16", "FAT16"},
		{0x36, "FAT12", "FAT12"},
		{0x36, "FAT", "FAT"},
	} {
		b, ok := r.at(c.off, 8)
		if ok && bytes.HasPrefix(b, []byte(c.want)) {
			return find{ContentForeign, "vfat", c.off,
				fmt.Sprintf("%s boot sector, type at %d", c.name, c.off)}, true
		}
	}
	return find{}, false
}

// detectExFAT matches exFAT, whose type string sits right after the jump
// instruction and which shares nothing else with the FAT family.
func detectExFAT(r regions) (find, bool) {
	if !r.eq(3, []byte("EXFAT   ")) {
		return find{}, false
	}
	return find{ContentForeign, "exfat", 3, "exFAT boot sector, type at 3"}, true
}

// detectBtrfs matches the Btrfs superblock, which sits well inside the head
// region at 64 KiB plus its own 64-byte offset.
func detectBtrfs(r regions) (find, bool) {
	const off = 0x10040
	if !r.eq(off, []byte("_BHRfS_M")) {
		return find{}, false
	}
	return find{ContentForeign, "btrfs", off, fmt.Sprintf("btrfs superblock at %d", off)}, true
}

// detectSwap matches a swap signature, which sits ten bytes before the end of
// the first page and therefore moves with the page size the area was made for.
func detectSwap(r regions) (find, bool) {
	for _, pageSize := range []int64{4096, 8192, 16384, 65536} {
		off := pageSize - 10
		for _, want := range []string{"SWAPSPACE2", "SWAP-SPACE"} {
			if r.eq(off, []byte(want)) {
				return find{ContentForeign, "swap", off,
					fmt.Sprintf("%s signature at %d, for a %d-byte page", want, off, pageSize)}, true
			}
		}
	}
	return find{}, false
}

// mdMagic is the software-RAID superblock magic, stored little-endian.
var mdMagic = []byte{0xfc, 0x4e, 0x2b, 0xa9}

// detectMDRaid matches the three metadata layouts by their documented
// locations: 1.1 at the start, 1.2 one block in, and 1.0 near the end.
//
// The 1.1 case is why this detector earns its place. blkid reports nothing at
// all for a metadata-1.1 member and exits 2, which is the same answer it gives
// for a blank device, on a device that is neither degraded nor unreadable.
func detectMDRaid(r regions) (find, bool) {
	type candidate struct {
		off  int64
		what string
	}
	cands := []candidate{
		{0, "metadata 1.1, at 0"},
		{4096, "metadata 1.2, at 4096"},
	}
	// 1.0 sits at least 8 KiB from the end, rounded down to a 4 KiB boundary.
	if sectors := r.size / 512; sectors > 16 {
		off := ((sectors - 16) &^ 7) * 512
		cands = append(cands, candidate{off, fmt.Sprintf("metadata 1.0, at %d", off)})
	}
	for _, c := range cands {
		if r.eq(c.off, mdMagic) {
			return find{ContentForeign, "linux_raid_member", c.off,
				"Linux software-RAID member, " + c.what}, true
		}
	}
	return find{}, false
}

// detectZFS matches a vdev label's uberblock magic. ZFS writes four labels, two
// at the start of the device and two at the end, and each one holds its
// uberblock array 128 KiB in.
func detectZFS(r regions) (find, bool) {
	const labelSize = 256 << 10
	const uberblockArray = 128 << 10
	starts := []int64{0, labelSize, r.size - 2*labelSize, r.size - labelSize}
	le := binary.LittleEndian.AppendUint64(nil, 0x0000000000bab10c)
	be := binary.BigEndian.AppendUint64(nil, 0x0000000000bab10c)
	for _, s := range starts {
		if s < 0 {
			continue
		}
		base := s + uberblockArray
		// The uberblocks are at least 1 KiB apart, so checking that stride
		// covers the array without scanning it byte by byte.
		for off := base; off < base+labelSize/2; off += 1024 {
			if r.eq(off, le) || r.eq(off, be) {
				return find{ContentForeign, "zfs_member", off,
					fmt.Sprintf("ZFS uberblock at %d, in the vdev label at %d", off, s)}, true
			}
		}
	}
	return find{}, false
}

// choose picks the find a reading reports from everything that was located.
//
// The lowest offset wins, because the layer a stack is looking for sits below
// whatever was written on top of it: a device carrying both an LVM label and a
// stale filesystem signature is the label's. GPT is the one exception, and it
// beats the protective MBR that is part of a well-formed GPT rather than a
// separate thing found on the same device.
func choose(finds []find) find {
	best := finds[0]
	for _, f := range finds[1:] {
		switch {
		case f.typ == "gpt" && best.typ == "dos":
			best = f
		case best.typ == "gpt" && f.typ == "dos":
			// keep the GPT
		case f.offset < best.offset:
			best = f
		}
	}
	return best
}
