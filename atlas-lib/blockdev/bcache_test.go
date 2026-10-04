// The one format in the catalog whose signature sits behind a zero first block.
//
// bcache puts a fast device in front of a slow one, and it is applied to a whole
// disk: make-bcache -B /dev/sdb. Its superblock starts at 4096, so the first
// block of a bcache device is zero and a reading that decided on the first block
// alone would call the disk free and hand it to a format.
//
// Nothing else in the catalog is shaped that way, and the two things people
// reach for alongside bcache are not: an lvmcache disk is an LVM physical volume
// and carries the label at 512, and a dm-cache metadata device has its
// superblock at offset 0. Both are captured beside this one so that the claim is
// read off a device rather than taken from a commit message.

package blockdev

import (
	"context"
	"testing"
)

// bcacheSuperblockAt is where make-bcache starts its superblock, measured on the
// capture. What the test needs from it is that it is past the first block.
const bcacheSuperblockAt = 4096

func TestABcacheDeviceIsNamedAlthoughItsFirstBlockIsZero(t *testing.T) {
	im := loadImage(t, "bcache")

	// What the device is, read off the capture rather than asserted about it.
	if !allZero(im.head[:bcacheSuperblockAt]) {
		t.Fatalf("the first %d bytes of this capture are not zero, so it is not the "+
			"shape this test covers", bcacheSuperblockAt)
	}
	if allZero(im.head[bcacheSuperblockAt : bcacheSuperblockAt+64]) {
		t.Fatalf("the block at %d is zero, so the capture no longer shows the superblock",
			bcacheSuperblockAt)
	}

	p := NewProberWithOpener(
		func(context.Context, Device) (Reader, error) { return im.Reader(), nil },
		WithRegionSize(DefaultRegionSize))
	got, err := p.Read(context.Background(), im.Device())
	if err != nil {
		t.Fatalf("Read(bcache): %v", err)
	}
	if got.Content == ContentBlank {
		t.Fatalf("Read(bcache).Content = Blank, which offers somebody's cache to a format")
	}
	if got.Type != "bcache" {
		t.Errorf("Read(bcache).Type = %q, want %q: the device is refused either way, but a "+
			"refusal that cannot say what it found is one nobody can act on\ndetail: %s",
			got.Type, "bcache", got.Detail)
	}
}

// The two caches people run beside bcache, each named by something that is
// already in the catalog or by its own first byte. These are the rows that say
// no further detector is owed, and they are read off captures for the same
// reason every other row is.
func TestTheOtherBlockCachesNeedNoSignatureOfTheirOwn(t *testing.T) {
	for _, tc := range []struct {
		image   string
		content Content
		typ     string
		why     string
	}{
		{"lvmcache", ContentStackLayer, "LVM2_member",
			"an lvmcache disk is an LVM physical volume, labelled at 512"},
		{"dm-cache-metadata", ContentForeign, "",
			"a dm-cache metadata device starts at a byte that is not zero"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			im := loadImage(t, tc.image)
			if allZero(im.head[:bcacheSuperblockAt]) {
				t.Fatalf("the first %d bytes are zero, so %s and this device would need a "+
					"detector of its own", bcacheSuperblockAt, tc.why)
			}
			p := NewProberWithOpener(
				func(context.Context, Device) (Reader, error) { return im.Reader(), nil },
				WithRegionSize(DefaultRegionSize))
			got, err := p.Read(context.Background(), im.Device())
			if err != nil {
				t.Fatalf("Read(%s): %v", tc.image, err)
			}
			if got.Content != tc.content {
				t.Errorf("Read(%s).Content = %s, want %s: %s", tc.image, got.Content, tc.content, tc.why)
			}
		})
	}
}

// What a wipe leaves, and what it means.
//
// wipefs erases the signatures libblkid knows and nothing else. On the ext4
// capture it took the two bytes at 0x438, reported success, and left the inode
// tables, the journal, and the files it was made with. blkid says nothing about
// the device afterward. That is not a disk that became empty; it is a disk
// somebody gave up, and the reading says so by naming the format whose signature
// went missing.
//
// The random capture is the other half of the pair and the reason the structural
// check is not optional. wipefs ran on it too, found nothing it knew, erased
// nothing, and reported success just the same. Nothing about that device says it
// was released, so it is refused.
func TestAWipedDeviceIsReleasedAndAWipedNothingIsNot(t *testing.T) {
	for _, tc := range []struct {
		image   string
		content Content
		typ     string
	}{
		{"wiped-ext4", ContentReleased, "ext4"},
		{"wiped-btrfs", ContentReleased, "btrfs"},
		{"wiped-bcache", ContentReleased, "bcache"},
		{"wiped-random", ContentForeign, ""},
	} {
		t.Run(tc.image, func(t *testing.T) {
			im := loadImage(t, tc.image)
			if im.Blkid != "" {
				t.Fatalf("blkid reports %q for this capture, so wipefs left a signature and "+
					"the catalog decides this device rather than the excision", im.Blkid)
			}
			p := NewProberWithOpener(
				func(context.Context, Device) (Reader, error) { return im.Reader(), nil },
				WithRegionSize(DefaultRegionSize))
			got, err := p.Read(context.Background(), im.Device())
			if err != nil {
				t.Fatalf("Read(%s): %v", tc.image, err)
			}
			if got.Content == ContentBlank {
				t.Errorf("Read(%s).Content = Blank: the device is not empty, whatever was done to it",
					tc.image)
			}
			if got.Content != tc.content {
				t.Errorf("Read(%s).Content = %s, want %s\ndetail: %s",
					tc.image, got.Content, tc.content, got.Detail)
			}
			if got.Type != tc.typ {
				t.Errorf("Read(%s).Type = %q, want %q", tc.image, got.Type, tc.typ)
			}
		})
	}
}

// The structural half of every excision check, put under the one pressure that
// matters: a run of zeros where a signature belongs, on a device that carries
// nothing else. If any check fired on that, every sufficiently empty disk would
// read as a disk somebody released.
func TestZerosWhereASignatureBelongsAreNotAnExcision(t *testing.T) {
	s := newSynth(64<<20, DefaultRegionSize)

	got, err := readSynth(t, s)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Content == ContentReleased {
		t.Fatalf("an all-zero device reads as %s, named %q: every offset a signature lives at "+
			"is zero on it, so a check that asked only about the zeros would fire here\ndetail: %s",
			got.Content, got.Type, got.Detail)
	}
	if got.Content != ContentBlank {
		t.Errorf("Read().Content = %s, want Blank for an all-zero device", got.Content)
	}
}
