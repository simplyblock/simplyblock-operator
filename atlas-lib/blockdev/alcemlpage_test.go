// What a storage node's page grid says about a device whose superblock is gone.
//
// The superblock at offset 0 is the whole of detectAlceml, and it is one 4096
// byte block: anything that zeroes it, by hand or by a release path, takes with
// it the only thing naming the device as this product's. What is left is a
// device covered in this product's own pages that the catalog reads as somebody
// else's bytes, and a fleet's capacity goes with it.
//
// The pages are the second name and the one that survives. This file reads a
// capture of a device in that state and asserts it is named rather than refused.

package blockdev

import (
	"context"
	"strings"
	"testing"
)

// The offsets the alceml-map-pages capture is read against, measured on the
// capture rather than decoded from a layout this package knows. What the tests
// need from them is that the grid exists and where its first header sits.
const (
	// mapPagesFirstPage is where the grid starts on the captured device, 12288
	// bytes past the end of a default head region. That gap is the whole reason
	// the device read as it did, and it is why the prober reaches past the head
	// for the grid rather than widening the head.
	mapPagesFirstPage = 1060864

	// mapPagesMapAt is the first byte of the mapping region, which is what
	// separates this capture from alceml-pages: there the whole span up to the
	// first page was zero and the head-only rule called the device released.
	// Here the map is still written, so that rule cannot.
	mapPagesMapAt = 4096
)

// Regression: 2026-09-30-alceml-map-without-a-superblock-reads-as-foreign.
//
// worker-4 of the OKD lab cluster on 2026-09-30, /dev/sda: the one disk of the
// fleet's twelve whose ALCEML_STORAGE superblock was gone. The other eleven
// carried theirs, were read as this product's own, and were taken. This one
// read as foreign, discovery declined it, and the cluster came up with worker-4
// contributing one 1.5 TB disk where every other worker contributed two, so the
// draft carried two node groups instead of one and the fleet is no longer
// uniform.
//
// It is not a state any code path here produces. The deployment that failed on
// this same lab hours earlier left all eleven other disks with their superblocks
// intact, so a failed node_add is not what does this: somebody zeroed 4096 bytes
// by hand. That is the point. How a disk gets blanked is not this package's to
// predict, and a reading that depends on the blanking gesture having reached a
// particular offset will keep being wrong in a new way.
func TestAnAlcemlDeviceWithItsMapIntactIsNamedAsOurs(t *testing.T) {
	im := loadImage(t, "alceml-map-pages")

	// What the device is, read off the capture rather than asserted about it.
	if got := string(im.head[0:16]); strings.Contains(got, "ALCEML") {
		t.Fatalf("offset 0 carries %q, so this capture still has its superblock "+
			"and is not the case this test covers", got)
	}
	if allZero(im.head[mapPagesMapAt : mapPagesMapAt+4096]) {
		t.Fatalf("the block at %d is zero, so this capture is alceml-pages over again "+
			"and the head-only rule would call the device blank", mapPagesMapAt)
	}
	block := im.head[mapPagesFirstPage : mapPagesFirstPage+4096]
	if got := string(block[:len(alcemlUnmappedMagic)]); got != string(alcemlUnmappedMagic) {
		t.Fatalf("offset %d carries %q, want %q: the capture no longer shows the page "+
			"header that proves whose device this is", mapPagesFirstPage, got, alcemlUnmappedMagic)
	}

	// What a host reads, at the region a host probes with.
	p := NewProberWithOpener(
		func(context.Context, Device) (Reader, error) { return im.Reader(), nil },
		WithRegionSize(DefaultRegionSize))
	got, err := p.Read(context.Background(), im.Device())
	if err != nil {
		t.Fatalf("Read(alceml-map-pages): %v", err)
	}
	if got.Content == ContentForeign {
		t.Fatalf("Read(alceml-map-pages).Content = Foreign: the device is covered in this "+
			"product's own pages and is being called somebody else's\ndetail: %s", got.Detail)
	}
	if got.Content != ContentSimplyblock {
		t.Errorf("Read(alceml-map-pages).Content = %s, want Simplyblock\ndetail: %s",
			got.Content, got.Detail)
	}
}

// A device carrying the word and nothing else about it is not this product's.
// The page header is a whole block, the magic and then zeros to the end of it,
// and that shape is what makes a ten-character word usable as a signature at
// all: the word alone would make every disk that happens to contain it ours to
// format.
func TestAWordOnItsOwnIsNotAPageHeader(t *testing.T) {
	s := newSynth(64<<20, DefaultRegionSize)
	write(s, mapPagesFirstPage, alcemlUnmappedMagic)
	write(s, mapPagesFirstPage+int64(len(alcemlUnmappedMagic)), []byte(" region of a backup index"))

	got, err := readSynth(t, s)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Content == ContentSimplyblock {
		t.Errorf("Read().Content = Simplyblock for a device carrying the word in prose, "+
			"which makes every disk with that word on it ours to format\ndetail: %s", got.Detail)
	}
}

// An unmapped page header found where the grid starts names the device, on a
// device built rather than captured, so that the rule is pinned apart from the
// one capture that happens to exercise it.
func TestAnUnmappedPageHeaderNamesTheDevice(t *testing.T) {
	s := newSynth(64<<20, DefaultRegionSize)
	write(s, 8192, []byte{1, 2, 3, 4}) // a non-zero head, so the zero rule cannot decide
	write(s, mapPagesFirstPage, alcemlUnmappedMagic)

	got, err := readSynth(t, s)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Content != ContentSimplyblock {
		t.Errorf("Read().Content = %s, want Simplyblock\ndetail: %s", got.Content, got.Detail)
	}
}

func write(s *synth, off int64, b []byte) {
	for i, c := range b {
		s.bytes[off+int64(i)] = c
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
