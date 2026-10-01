// A signature that was deliberately taken out, and the format it was taken from.
//
// wipefs is how a disk is released. It is root, it names one device, and its
// whole purpose is to make the device reusable, and what it does to achieve
// that is erase the few bytes that name the format, and nothing else. On an ext4
// filesystem that is two bytes. Everything else stays: the superblock body, the
// inode tables, the journal, the files.
//
// So a released disk does not look empty, and a rule that asks it to is asking
// for a gesture nobody performs. Zeroing 1.5 TB is hours, and zeroing some arbitrary
// prefix of it is what people actually do and how much they zero is whatever
// they typed. Neither is what the tool built for this does.
//
// What this file reads instead is the excision: the format is still legible from
// its own structure, and the word naming it is gone from the one place it lives.
// That is not a state a device reaches by accident or by wear. It is the mark of
// somebody having run the tool, and it means the same thing on every format,
// whatever the head happens to look like afterward.
//
// Each check is read off a capture under testdata/images/wiped-*, made by
// running the real tool and then the real wipefs. The structural half is never
// omitted: zeros at an offset are a coincidence a large enough disk will supply,
// and it is the surviving structure that makes the absence mean something.

package blockdev

import (
	"encoding/binary"
	"fmt"
)

// excision is one format's signature, where wipefs takes it from, and what has
// to still be there for its absence to mean the disk was given up.
type excision struct {
	// typ names the format in the spelling the catalog uses for it.
	typ string

	// member names the exact format where a family shares one signature and the
	// structure that survives still says which member it was. It is nil for a
	// format that is only ever itself.
	member func(regions) string

	// erased reports where the signature belongs and whether it is gone. A
	// format whose signature can be in more than one place answers for the one
	// it found the structure at.
	erased func(regions) (int64, bool)

	// intact reports whether the format is still legible around that offset. It
	// is the half that stops a run of zeros from reading as a released disk.
	intact func(regions) bool
}

// excisions is the catalog's second half: one entry per format whose signature
// wipefs knows how to erase. A format missing from here is not misread, it is
// only refused where it could have been released, so adding one is a capture and
// a row rather than a change to how anything is decided.
var excisions = []excision{
	{
		typ: "ext",
		// The feature words outlive the magic, and they are the whole of how the
		// catalog tells the family apart in the first place.
		member: func(r regions) string { return extMember(r) },
		erased: zeroedAt(extMagicOffset, 2),
		// The superblock body survives whole: wipefs takes the two magic bytes
		// at 1080 and leaves the counts around them. A block size above 6 is
		// past what ext supports, and a filesystem with no inodes or no blocks
		// was never made.
		intact: func(r regions) bool {
			inodes, ok1 := r.u32(extSuperblock)
			blocks, ok2 := r.u32(extSuperblock + 4)
			logBlockSize, ok3 := r.u32(extSuperblock + 24)
			return ok1 && ok2 && ok3 && inodes != 0 && blocks != 0 && logBlockSize <= 6
		},
	},
	{
		typ:    "xfs",
		erased: zeroedAt(0, 4),
		// sb_blocksize follows the magic and is big-endian, as everything in an
		// XFS superblock is.
		intact: func(r regions) bool { return powerOfTwoBE(r, 4, 512, 65536) },
	},
	{
		typ:    "btrfs",
		erased: zeroedAt(0x10040, 8),
		// The superblock starts 64 bytes before the magic with its checksum,
		// and the first 64 KiB of the device is reserved and stays zero, so a
		// non-zero checksum there is the filesystem and not the bootloader.
		intact: func(r regions) bool { return !zeroSpan(r, 0x10000, 32) },
	},
	{
		typ:    "bcache",
		erased: zeroedAt(4096+24, 16),
		// The superblock opens with a checksum, then the sector it lives at,
		// which is always 8, and then the version.
		intact: func(r regions) bool {
			offset, ok := r.u64(4096 + 8)
			return ok && offset == 8 && !zeroSpan(r, 4096, 8)
		},
	},
	{
		typ:    "swap",
		erased: swapErased,
		// The header is at the start of the first page and the signature at the
		// end of it, so the two are far apart and only the signature goes.
		intact: func(r regions) bool {
			version, ok1 := r.u32(1024)
			lastPage, ok2 := r.u32(1028)
			return ok1 && ok2 && version == 1 && lastPage != 0
		},
	},
	{
		typ: "LVM2_member",
		// The label keeps its own name and loses the type that follows it,
		// which is the field detectLVM2 reads second.
		erased: zeroedAt(512+24, 8),
		intact: func(r regions) bool { return r.eq(512, []byte("LABELONE")) },
	},
	{
		typ:    "crypto_LUKS",
		erased: zeroedAt(0, 6),
		intact: func(r regions) bool {
			b, ok := r.at(6, 2)
			if !ok {
				return false
			}
			v := binary.BigEndian.Uint16(b)
			return v == 1 || v == 2
		},
	},
	{
		typ:    "exfat",
		erased: zeroedAt(3, 8),
		// The jump instruction opens the boot sector and wipefs leaves it.
		intact: func(r regions) bool { return r.eq(0, []byte{0xEB, 0x76, 0x90}) },
	},
	{
		typ:    "vfat",
		erased: fatErased,
		// The BIOS parameter block is the structure the catalog already reads to
		// decide a boot sector is a boot sector, and wipefs takes only the type
		// string out of it.
		intact: validFATBPB,
	},
	{
		typ:    "gpt",
		erased: gptErased,
		// The protective MBR entry stays, and its type byte is what makes it
		// protective rather than a partition somebody meant.
		intact: func(r regions) bool {
			b, ok := r.at(446+4, 1)
			return ok && b[0] == 0xEE
		},
	},
	{
		typ:    "dos",
		erased: zeroedAt(510, 2),
		// A table with a used entry, which is what detectMBR asks for as well:
		// two zero bytes at 510 on a disk with no partitions is just zeros.
		intact: func(r regions) bool {
			table, ok := r.at(446, 64)
			if !ok {
				return false
			}
			for i := range 4 {
				if table[i*16+4] != 0 {
					return true
				}
			}
			return false
		},
	},
	{
		typ:    "linux_raid_member",
		erased: mdErased,
		intact: mdIntact,
	},
}

// detectExcised names a format whose signature was erased where it lives, with
// the rest of the format still on the device.
func detectExcised(r regions) (find, bool) {
	for _, e := range excisions {
		off, gone := e.erased(r)
		if !gone || !e.intact(r) {
			continue
		}
		typ := e.typ
		if e.member != nil {
			typ = e.member(r)
		}
		return find{ContentReleased, typ, off, fmt.Sprintf(
			"%s: the signature is gone from %d and the rest of the format is still on the "+
				"device, which is the mark of wipefs and how a disk is released", typ, off)}, true
	}
	return find{}, false
}

// zeroedAt reports a fixed span as erased when every byte of it was read and is
// zero. A span that was not read is not erased: it is unknown, and this answers
// only what it saw.
func zeroedAt(off, n int64) func(regions) (int64, bool) {
	return func(r regions) (int64, bool) { return off, zeroSpan(r, off, n) }
}

// zeroSpan reports whether the n bytes at off were read and are all zero.
func zeroSpan(r regions, off, n int64) bool {
	b, ok := r.at(off, n)
	if !ok {
		return false
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// powerOfTwoBE reports whether the big-endian word at off is a power of two
// within the bounds, which is how a block size is checked without decoding the
// rest of a superblock this package does not read.
func powerOfTwoBE(r regions, off int64, low, high uint32) bool {
	b, ok := r.at(off, 4)
	if !ok {
		return false
	}
	v := binary.BigEndian.Uint32(b)
	return v >= low && v <= high && v&(v-1) == 0
}

// swapErased answers for whichever page size the header says the device was
// made with, because the signature sits at the end of the first page and the
// page size is the system's rather than the format's.
func swapErased(r regions) (int64, bool) {
	for _, pageSize := range []int64{4096, 8192, 16384, 65536} {
		if zeroSpan(r, pageSize-10, 10) {
			return pageSize - 10, true
		}
	}
	return 0, false
}

// fatErased answers for the type field of whichever FAT this is. FAT32 keeps it
// at 0x52 and the shorter ones at 0x36, and a boot sector has one or the other.
func fatErased(r regions) (int64, bool) {
	for _, off := range []int64{0x52, 0x36} {
		if zeroSpan(r, off, 8) {
			return off, true
		}
	}
	return 0, false
}

// gptErased answers for the primary header, at LBA 1 whatever the block size is.
func gptErased(r regions) (int64, bool) {
	return r.lbs, zeroSpan(r, r.lbs, 8)
}

// mdErased and mdIntact answer for the offset the metadata version puts the
// superblock at, which is the same set detectMDRaid tries.
func mdErased(r regions) (int64, bool) {
	for _, off := range mdOffsets(r) {
		if zeroSpan(r, off, 4) && mdVersionAt(r, off) {
			return off, true
		}
	}
	return 0, false
}

func mdIntact(r regions) bool {
	for _, off := range mdOffsets(r) {
		if zeroSpan(r, off, 4) && mdVersionAt(r, off) {
			return true
		}
	}
	return false
}

// mdVersionAt reports whether the words after the magic are a version an md
// superblock carries. The 1.x layouts keep a major of 1 there. The 0.90 layout
// keeps a major of 0, which on its own is indistinguishable from the zeros a
// wipe leaves, so it is read together with the minor of 90 that follows it.
func mdVersionAt(r regions, off int64) bool {
	major, ok := r.u32(off + 4)
	if !ok {
		return false
	}
	if major == 1 {
		return true
	}
	minor, ok := r.u32(off + 8)
	return ok && major == 0 && minor == 90
}

func mdOffsets(r regions) []int64 {
	offs := []int64{0, 4096}
	if sectors := r.size / 512; sectors > 16 {
		offs = append(offs, ((sectors-16)&^7)*512)
	}
	if sectors := r.size / 512; sectors > 128 {
		offs = append(offs, ((sectors&^127)-128)*512)
	}
	return offs
}
