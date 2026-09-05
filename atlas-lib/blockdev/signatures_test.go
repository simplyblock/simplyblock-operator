// Signature edge cases a captured image cannot show.
//
// Every capture is one device a tool wrote one way. These are the variants of
// the same formats that a real tool will not produce on request: a label in a
// sector other than the one LVM happens to use, a boot signature with nothing
// behind it, and two formats on one device.

package blockdev

import (
	"context"
	"strings"
	"testing"
)

// place writes b at off in the synthetic device.
func (s *synth) place(off int64, b []byte) {
	for i, c := range b {
		s.bytes[off+int64(i)] = c
	}
}

func readOf(t *testing.T, s *synth) Reading {
	t.Helper()
	got, err := s.prober().Read(context.Background(), s.device())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return got
}

// lvmLabel writes a physical-volume label into the sector at off.
func lvmLabel(s *synth, off int64) {
	s.place(off, []byte("LABELONE"))
	s.place(off+24, []byte("LVM2 001"))
}

// U-06: LVM scans the first four 512-byte units, so a label in any of them is a
// label. Real pvcreate uses the second, which is the only one a capture shows.
func TestLVMLabelInAnyOfTheFirstFourSectors(t *testing.T) {
	for _, sector := range []int64{0, 1, 2, 3} {
		t.Run(string(rune('0'+sector)), func(t *testing.T) {
			s := newSynth(8<<20, MinRegionSize)
			lvmLabel(s, sector*512)

			got := readOf(t, s)
			if got.Content != ContentStackLayer {
				t.Fatalf("Content = %s, want StackLayer for a label in sector %d", got.Content, sector)
			}
			if got.Type != "LVM2_member" {
				t.Errorf("Type = %q, want LVM2_member", got.Type)
			}
		})
	}
}

// U-07: the eight bytes are a label header, not a label. Without the LVM2 type
// behind them the device is not a physical volume, and calling it one would put
// a device into the attach path that has nothing to attach.
func TestLabelWithoutTheLVMTypeIsNotAStackLayer(t *testing.T) {
	s := newSynth(8<<20, MinRegionSize)
	s.place(512, []byte("LABELONE"))
	s.place(512+24, []byte("SOMEONE1"))

	got := readOf(t, s)
	if got.Content == ContentStackLayer {
		t.Fatal("a label header with a foreign type read as an LVM physical volume")
	}
	if got.Content != ContentForeign {
		t.Errorf("Content = %s, want Foreign: the bytes are there, they are just not LVM's", got.Content)
	}
}

// U-11: the boot signature alone is not a partition table. A device whose first
// sector ends in 0x55AA with an empty table is refused for its bytes rather than
// named as something it is not.
func TestBootSignatureWithAnEmptyTableIsNotAPartitionTable(t *testing.T) {
	s := newSynth(8<<20, MinRegionSize)
	s.place(510, []byte{0x55, 0xAA})

	got := readOf(t, s)
	if got.Type == "dos" {
		t.Fatal("an empty partition table was named as an MBR")
	}
	if got.Content != ContentForeign {
		t.Errorf("Content = %s, want Foreign", got.Content)
	}
}

// U-14: a device can carry a stack layer and the remains of a filesystem that
// was on it before. The layer wins because it sits lower, and the refusal names
// both so an operator is not told half of what is there.
func TestLVMLabelBeatsAStaleFilesystemAndBothAreNamed(t *testing.T) {
	s := newSynth(8<<20, MinRegionSize)
	lvmLabel(s, 512)
	// An ext4 superblock left behind at 1024, above the label.
	s.place(extMagicOffset, []byte{0x53, 0xEF})
	s.place(extSuperblock+96, []byte{0x40, 0x00, 0x00, 0x00}) // the extents feature

	got := readOf(t, s)
	if got.Content != ContentStackLayer {
		t.Fatalf("Content = %s, want StackLayer: the label at 512 sits below the superblock at 1024",
			got.Content)
	}
	if !strings.Contains(got.Detail, "LVM2") || !strings.Contains(got.Detail, "ext4") {
		t.Errorf("Detail names only part of what is on the device: %s", got.Detail)
	}
}

// U-58: FAT has no single magic, so a boot sector whose BIOS parameter block is
// not a BIOS parameter block is not FAT. It is still refused, because the bytes
// that made it look like FAT are themselves content.
func TestFATTypeStringWithoutAValidBPBIsNotFAT(t *testing.T) {
	s := newSynth(8<<20, MinRegionSize)
	s.place(0x36, []byte("FAT16   "))
	// A bytes-per-sector of 3 is not one of the four the format allows.
	s.place(0x0B, []byte{0x03, 0x00})

	got := readOf(t, s)
	if got.Type == "vfat" {
		t.Fatal("a boot sector with an impossible bytes-per-sector was named as FAT")
	}
	if got.Content != ContentForeign {
		t.Errorf("Content = %s, want Foreign", got.Content)
	}
}

// U-61: a GPT whose primary header is gone is still a GPT, because the format
// keeps a second copy in the last block. A device read only at its head would
// call this one blank.
func TestGPTFoundByItsBackupHeaderAlone(t *testing.T) {
	const size = 8 << 20
	s := newSynth(size, MinRegionSize)
	s.place(size-512, []byte("EFI PART")) // the last LBA, with 512-byte blocks

	got := readOf(t, s)
	if got.Content != ContentForeign {
		t.Fatalf("Content = %s, want Foreign", got.Content)
	}
	if got.Type != "gpt" {
		t.Errorf("Type = %q, want gpt: the backup header is at the last LBA", got.Type)
	}
	if !strings.Contains(got.Detail, "backup") {
		t.Errorf("Detail does not say the backup header was what matched: %s", got.Detail)
	}
}

// The GPT offsets follow the device's logical block size, so the same signature
// is at a different place on a 4Kn device. This is the arithmetic the captured
// gpt-4kn image proves on one device and this covers for both.
func TestGPTOffsetFollowsTheLogicalBlockSize(t *testing.T) {
	for _, lbs := range []uint32{512, 4096} {
		t.Run(string(rune('0'+lbs/4096)), func(t *testing.T) {
			s := newSynth(8<<20, MinRegionSize)
			s.place(int64(lbs), []byte("EFI PART"))

			dev := s.device()
			dev.LogicalBlockSize = lbs
			got, err := s.prober().Read(context.Background(), dev)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got.Type != "gpt" {
				t.Fatalf("Type = %q, want gpt: LBA 1 is offset %d on this device", got.Type, lbs)
			}
		})
	}
}
