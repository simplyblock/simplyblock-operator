// What the reading must report for each captured device image.
//
// Every row here is an assertion about a device a real tool formatted, so a row
// that fails means the reading disagrees with what mkfs, pvcreate, sgdisk, or
// mdadm actually wrote, rather than with a table somebody typed.

package blockdev

import (
	"context"
	"strings"
	"testing"
)

// want is the reading one captured image must produce.
type want struct {
	image    string
	content  Content
	fsType   string // the Type the reading must name, when it names one
	scenario string
}

// The catalog, one row per capture. The fsType column is the spelling a
// consumer acts on: a mount type for a filesystem, the label name for a stack
// layer, and the format's own name for something foreign.
var catalog = []want{
	// U-01, U-02, U-03: the ext family, decoded to its exact member.
	{"ext2", ContentFilesystem, "ext2", "U-01/U-03: an ext2 superblock names ext2"},
	{"ext3", ContentFilesystem, "ext3", "U-02: a journal without the ext4 features names ext3"},
	{"ext4", ContentFilesystem, "ext4", "U-01: the ext4 feature words name ext4"},
	// U-04.
	{"xfs", ContentFilesystem, "xfs", "U-04: an XFS superblock at offset 0"},
	// U-05: the one reading that is a layer rather than a filesystem.
	{"lvm2", ContentStackLayer, "LVM2_member", "U-05: a physical-volume label"},
	// U-08.
	{"luks1", ContentForeign, "crypto_LUKS", "U-08: a LUKS1 header"},
	{"luks2", ContentForeign, "crypto_LUKS", "U-08: a LUKS2 header"},
	// U-09, U-59, U-60.
	{"gpt", ContentForeign, "gpt", "U-09/U-59: a GPT, reported as GPT and not as its protective MBR"},
	{"gpt-4kn", ContentForeign, "gpt", "U-60: a GPT on a 4Kn device, whose header is at 4096"},
	// U-10.
	{"mbr", ContentForeign, "dos", "U-10: an MBR partition table"},
	// U-54, U-55, U-56.
	{"fat12", ContentForeign, "vfat", "U-54: FAT12"},
	{"fat16", ContentForeign, "vfat", "U-54: FAT16"},
	{"fat32", ContentForeign, "vfat", "U-55: FAT32"},
	{"exfat", ContentForeign, "exfat", "U-56: exFAT"},
	// U-12.
	{"btrfs", ContentForeign, "btrfs", "U-12: a btrfs superblock at 65600"},
	{"swap", ContentForeign, "swap", "U-12: a swap signature near the end of the first page"},
	{"mdraid-10", ContentForeign, "linux_raid_member", "U-12: md metadata 1.0, whose superblock is in the tail region"},
	{"mdraid-11", ContentForeign, "linux_raid_member", "U-12: md metadata 1.1, at offset 0"},
	{"mdraid-12", ContentForeign, "linux_raid_member", "U-12: md metadata 1.2, at offset 4096"},
	{"zfs", ContentForeign, "zfs_member", "U-12: ZFS vdev labels"},
	// The one capture no local tool can reproduce: the superblock is written by
	// a storage node, so the image comes off a device this product wrote.
	{"alceml", ContentSimplyblock, "simplyblock_alceml",
		"U-65: an alceml superblock, which blkid and wipefs both read as nothing"},
	// The same product's device without the superblock that names it. The row
	// records a miss rather than a target: see the test below for what the
	// device carries and for why the reading cannot reach it.
	{"alceml-pages", ContentForeign, "",
		"U-65: an alceml device whose superblock is gone reads as bytes matching nothing"},
	// U-15: the only reading that permits a format.
	{"blank", ContentBlank, "", "U-15: a device that has never been written to"},
}

func TestReadingOfCapturedImages(t *testing.T) {
	for _, w := range catalog {
		t.Run(w.image, func(t *testing.T) {
			im := loadImage(t, w.image)
			p := NewProberWithOpener(
				func(context.Context, Device) (Reader, error) { return im.Reader(), nil },
				WithRegionSize(im.RegionSize),
			)

			got, err := p.Read(context.Background(), im.Device())
			if err != nil {
				t.Fatalf("%s\nRead(%s) returned an error: %v\nthe image was written by: %s (%s)",
					w.scenario, w.image, err, im.Tool, im.ToolVersion)
			}
			if got.Content != w.content {
				t.Errorf("%s\nRead(%s).Content = %s, want %s\nblkid saw: %s",
					w.scenario, w.image, got.Content, w.content, im.Blkid)
			}
			if w.fsType != "" && got.Type != w.fsType {
				t.Errorf("%s\nRead(%s).Type = %q, want %q", w.scenario, w.image, got.Type, w.fsType)
			}
			if w.content == ContentBlank && got.Type != "" {
				t.Errorf("%s\nRead(%s).Type = %q, want empty for a blank device",
					w.scenario, w.image, got.Type)
			}
		})
	}
}

// U-13, U-16, U-17: the rule that makes the catalog's completeness a matter of
// message quality rather than of safety. Whatever the reading names a device,
// only a device positively read as all zeros may be formatted.
func TestOnlyTheBlankImageIsBlank(t *testing.T) {
	for _, name := range imageNames(t) {
		t.Run(name, func(t *testing.T) {
			im := loadImage(t, name)
			p := NewProberWithOpener(
				func(context.Context, Device) (Reader, error) { return im.Reader(), nil },
				WithRegionSize(im.RegionSize),
			)

			got, err := p.Read(context.Background(), im.Device())
			if err != nil {
				t.Fatalf("Read(%s): %v", name, err)
			}
			if name == "blank" {
				if got.Content != ContentBlank {
					t.Errorf("Read(%s).Content = %s, want Blank", name, got.Content)
				}
				return
			}
			if got.Content == ContentBlank {
				t.Errorf("Read(%s).Content = Blank, but this device carries a %s written by %s.\n"+
					"Formatting it would destroy what is on it",
					name, im.Tool, im.ToolVersion)
			}
		})
	}
}

// Regression: 2026-09-05-md-1-1-reads-as-unformatted. mdadm writes a valid
// metadata-1.1 superblock at offset 0, and blkid reports nothing for it and
// exits 2, which the driver reads as a device that is safe to format. The device
// is healthy and on a healthy host, so no fabric fault is involved: this is
// blkid's recognition coverage rather than its failure handling. Confirmed on
// Ubuntu 24.04 with mdadm 4.3 and util-linux 2.39.3, where `mdadm --examine`
// reports the magic on the same device blkid calls empty.
func TestMdMetadata11IsNotBlankEvenThoughBlkidSaysNothing(t *testing.T) {
	im := loadImage(t, "mdraid-11")

	if im.Blkid != "" {
		t.Skipf("blkid reported %q for this capture, so the conflation it pins is not reproduced here", im.Blkid)
	}
	if !strings.Contains(im.Note, "1.1") {
		t.Logf("capture note: %q", im.Note)
	}

	p := NewProberWithOpener(
		func(context.Context, Device) (Reader, error) { return im.Reader(), nil },
		WithRegionSize(im.RegionSize),
	)
	got, err := p.Read(context.Background(), im.Device())
	if err != nil {
		t.Fatalf("Read(mdraid-11): %v", err)
	}
	if got.Content == ContentBlank {
		t.Fatal("the reading calls an md 1.1 member blank, which is what blkid does and what this package exists to stop")
	}
	if got.Content != ContentForeign {
		t.Errorf("Read(mdraid-11).Content = %s, want Foreign", got.Content)
	}
}

// U-29: the zero value authorizes nothing, so a Reading nobody populated cannot
// be mistaken for permission to format.
func TestZeroReadingAuthorizesNothing(t *testing.T) {
	var r Reading
	if r.Content != ContentUnknown {
		t.Fatalf("the zero Reading has Content %s, want Unknown", r.Content)
	}
	if r.Content == ContentBlank {
		t.Fatal("the zero Reading reads as Blank, which makes an unpopulated struct a permission to mkfs")
	}
}

// The offsets the alceml-pages capture is read against. They are the device's,
// measured on the capture, and not a layout this package decodes: what the test
// needs from them is that the page grid exists and where it starts.
const (
	// alcemlPageMagic opens every page a storage node writes. It is not the
	// superblock magic: the superblock names the device once at offset 0, and
	// this repeats with the grid.
	alcemlPageMagic = "ALCEML_PAGEv2"

	// alcemlFirstPage is where the grid starts on the captured device. It is
	// 12288 bytes past the end of a default head region, which is the whole
	// reason the device reads as it does. The stride to the next page is
	// 2105344 and is recorded in the capture's manifest rather than here: the
	// second page is past the captured region, so no test can check it.
	alcemlFirstPage = 1060864
)

// A simplyblock device whose superblock has been wiped reads as foreign, and the
// pages that would have named it start just past where the head region stops.
//
// Every 1.5 TB disk of all four workers of the OKD lab cluster read this way on
// 2026-09-28, so a four-node deployment came up with one 30 GB disk per worker,
// and every storage node then refused to configure itself, because lblk mode
// needs at least two disks per node. The disks are not somebody else's: they
// carry a previous deployment's data, which is why declining them was right,
// and the refusal that declined them could not say so, which is why this test
// exists.
//
// It asserts the miss rather than the fix. Naming the device would take a
// detector for the page grid, and that detector has to be written against these
// bytes rather than against the offsets below, or it would assert only that it
// agrees with them.
func TestAnAlcemlDeviceWithoutItsSuperblockReadsAsForeign(t *testing.T) {
	im := loadImage(t, "alceml-pages")

	// What the device is, read off the capture rather than asserted about it.
	if got := string(im.head[0:16]); strings.Contains(got, "ALCEML") {
		t.Fatalf("offset 0 carries %q, so this capture still has its superblock "+
			"and is not the case this test covers", got)
	}
	if got := string(im.head[alcemlFirstPage : alcemlFirstPage+len(alcemlPageMagic)]); got != alcemlPageMagic {
		t.Errorf("offset %d carries %q, want %q: the capture no longer shows the page "+
			"that proves whose device this is", alcemlFirstPage, got, alcemlPageMagic)
	}

	// What the reading reports, at the region a probe on a host actually uses.
	p := NewProberWithOpener(
		func(context.Context, Device) (Reader, error) { return im.Reader(), nil },
		WithRegionSize(DefaultRegionSize))
	got, err := p.Read(context.Background(), im.Device())
	if err != nil {
		t.Fatalf("Read(alceml-pages): %v", err)
	}
	if got.Content == ContentBlank {
		t.Fatal("the reading calls a device holding a previous deployment's data blank, " +
			"and formatting it would destroy what is on it")
	}
	if got.Content != ContentForeign {
		t.Errorf("Read(alceml-pages).Content = %s, want Foreign.\n"+
			"Simplyblock would be the better answer, and reaching it takes a detector for %q "+
			"rather than a change here",
			got.Content, alcemlPageMagic)
	}

	// The refusal the cluster produced, to the byte. The first non-zero byte is
	// in the tail because the head region stops short of the first page.
	const wantDetail = "no known signature, and the probed regions are not empty: " +
		"first non-zero byte at 1610611687425"
	if got.Detail != wantDetail {
		t.Errorf("Read(alceml-pages).Detail = %q,\nwant %q\n"+
			"this is the sentence every worker's disk was declined with", got.Detail, wantDetail)
	}

	// The reason the head says nothing: the grid begins past its last byte.
	if alcemlFirstPage <= DefaultRegionSize {
		t.Errorf("the first page is at %d, inside a %d-byte head region, "+
			"so the head region is not what keeps the reading from naming this device",
			alcemlFirstPage, DefaultRegionSize)
	}
}
