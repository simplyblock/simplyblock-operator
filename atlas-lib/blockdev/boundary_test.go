// The zero rule and the read failures, at their boundaries.
//
// These inputs are constructed rather than captured, because what they test is a
// property of the rule rather than of any format: no image a real tool writes
// will place a byte at exactly the last offset of a region on request, and no
// healthy device will fail a read halfway through one.

package blockdev

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// synth is a device built byte by byte, with the read behavior a test needs.
type synth struct {
	size   int64
	region int64
	bytes  map[int64]byte // sparse: everything unset is zero

	failAt   func(off int64) error
	short    bool
	blockFor time.Duration

	reads []readRecord
}

type readRecord struct {
	off int64
	n   int64
}

func (s *synth) device() Device {
	return Device{
		Path:             "/dev/synthetic",
		Name:             "synthetic",
		LogicalBlockSize: 512,
		SizeBytes:        uint64(s.size),
	}
}

func (s *synth) prober(opts ...Option) *Prober {
	opts = append([]Option{WithRegionSize(s.region)}, opts...)
	return NewProberWithOpener(func(context.Context, Device) (Reader, error) { return s, nil }, opts...)
}

func (s *synth) Close() error { return nil }

func (s *synth) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	s.reads = append(s.reads, readRecord{off, int64(len(p))})

	if s.blockFor > 0 {
		select {
		case <-time.After(s.blockFor):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.failAt != nil {
		if err := s.failAt(off); err != nil {
			return 0, err
		}
	}
	for i := range p {
		p[i] = s.bytes[off+int64(i)]
	}
	if s.short {
		return len(p) - 1, nil
	}
	return len(p), nil
}

// newSynth builds an all-zero device of size with the given region size.
func newSynth(size, region int64) *synth {
	return &synth{size: size, region: region, bytes: map[int64]byte{}}
}

func readSynth(t *testing.T, s *synth, opts ...Option) (Reading, error) {
	t.Helper()
	return s.prober(opts...).Read(context.Background(), s.device())
}

// U-15: an all-zero device is the only reading that permits a format.
func TestBlankRequiresBothRegionsZero(t *testing.T) {
	s := newSynth(8<<20, MinRegionSize)
	got, err := readSynth(t, s)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Content != ContentBlank {
		t.Fatalf("Content = %s, want Blank (detail: %s)", got.Content, got.Detail)
	}
}

// U-16, U-17, U-18, U-19: one byte anywhere in either region defeats blank, and
// the two ends of each region are where an off-by-one would hide.
func TestOneNonZeroByteDefeatsBlank(t *testing.T) {
	const size = 8 << 20
	region := int64(MinRegionSize)

	cases := []struct {
		name string
		off  int64
	}{
		{"U-16: first byte of the head region", 0},
		{"U-16: inside the head region", 1234},
		{"U-18: last byte of the head region", region - 1},
		{"U-19: first byte of the tail region", size - region},
		{"U-17: inside the tail region", size - 1234},
		{"U-17: last byte of the device", size - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSynth(size, region)
			s.bytes[tc.off] = 0x01

			got, err := readSynth(t, s)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got.Content == ContentBlank {
				t.Fatalf("a byte at %d left the device reading Blank, so a format would be permitted", tc.off)
			}
			if got.Content != ContentForeign {
				t.Errorf("Content = %s, want Foreign", got.Content)
			}
		})
	}
}

// U-20: the rule is about the regions that are read, and a byte between them is
// not one of them. This is the reading's stated reach rather than an oversight:
// every format in the catalog writes into a region, and a device holding data
// only in the middle is one no tool this driver runs produces.
func TestAByteBetweenTheRegionsIsNotRead(t *testing.T) {
	const size = 8 << 20
	region := int64(MinRegionSize)

	s := newSynth(size, region)
	s.bytes[size/2] = 0xFF

	got, err := readSynth(t, s)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Content != ContentBlank {
		t.Fatalf("Content = %s, want Blank: the byte at %d is outside both regions", got.Content, size/2)
	}
	for _, r := range s.reads {
		if size/2 >= r.off && size/2 < r.off+r.n {
			t.Fatalf("the read at %d..%d covered the middle byte, so this test proves nothing",
				r.off, r.off+r.n)
		}
	}
}

// U-21: a device smaller than two regions is read once, whole, rather than read
// twice with an overlap that would double-count its bytes.
func TestSmallDeviceIsReadWhole(t *testing.T) {
	region := int64(MinRegionSize)
	s := newSynth(region+1024, region)
	s.bytes[region+512] = 0x01 // in the overlap a two-read device would have

	got, err := readSynth(t, s)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(s.reads) != 1 {
		t.Fatalf("made %d reads, want 1 for a device smaller than two regions: %+v", len(s.reads), s.reads)
	}
	if s.reads[0].off != 0 || s.reads[0].n != s.size {
		t.Errorf("read %d bytes at %d, want the whole %d-byte device", s.reads[0].n, s.reads[0].off, s.size)
	}
	if got.Content != ContentForeign {
		t.Errorf("Content = %s, want Foreign: the byte at %d was read", got.Content, region+512)
	}
}

// U-22: a device of exactly two regions is read as two regions that meet
// without overlapping.
func TestExactlyTwoRegionsDoNotOverlap(t *testing.T) {
	region := int64(MinRegionSize)
	s := newSynth(2*region, region)

	if _, err := readSynth(t, s); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(s.reads) != 1 {
		t.Fatalf("made %d reads, want 1: at exactly two regions the device is still read whole: %+v",
			len(s.reads), s.reads)
	}
}

// U-22: one byte past two regions, the two reads meet exactly.
func TestJustOverTwoRegionsReadsBothEnds(t *testing.T) {
	region := int64(MinRegionSize)
	s := newSynth(2*region+1, region)

	if _, err := readSynth(t, s); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(s.reads) != 2 {
		t.Fatalf("made %d reads, want 2: %+v", len(s.reads), s.reads)
	}
	head, tail := s.reads[0], s.reads[1]
	if head.off != 0 || head.n != region {
		t.Errorf("head read %d bytes at %d, want %d at 0", head.n, head.off, region)
	}
	if tail.off != s.size-region || tail.n != region {
		t.Errorf("tail read %d bytes at %d, want %d at %d", tail.n, tail.off, region, s.size-region)
	}
	if head.off+head.n > tail.off {
		t.Errorf("the regions overlap: head ends at %d, tail starts at %d", head.off+head.n, tail.off)
	}
}

// U-23, U-30: a device with no size has no locatable tail, so it is a read
// failure rather than a device that happens to be empty.
func TestZeroSizedDeviceIsAFailureNotBlank(t *testing.T) {
	s := newSynth(0, MinRegionSize)
	got, err := readSynth(t, s)
	if err == nil {
		t.Fatalf("Read returned %s with no error for a zero-sized device", got.Content)
	}
	if got.Content == ContentBlank {
		t.Fatal("a zero-sized device read as Blank")
	}
}

// U-24, U-25, U-26, U-27, U-28: every way a read can fail is an error, and none
// of them is ever a blank reading. This is the property the whole package
// exists to hold.
func TestEveryReadFailureIsAnErrorAndNeverBlank(t *testing.T) {
	const size = 8 << 20
	region := int64(MinRegionSize)
	eio := errors.New("input/output error")

	cases := []struct {
		name    string
		prepare func(*synth)
		opts    []Option
	}{
		{"U-24: the head region fails", func(s *synth) {
			s.failAt = func(off int64) error {
				if off == 0 {
					return eio
				}
				return nil
			}
		}, nil},
		{"U-25: the tail region fails", func(s *synth) {
			s.failAt = func(off int64) error {
				if off != 0 {
					return eio
				}
				return nil
			}
		}, nil},
		{"U-24: every read fails, as on a device with no path", func(s *synth) {
			s.failAt = func(int64) error { return eio }
		}, nil},
		{"U-26: the read never returns in time", func(s *synth) {
			s.blockFor = time.Hour
		}, []Option{WithTimeout(20 * time.Millisecond)}},
		{"U-27: the read is short", func(s *synth) { s.short = true }, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSynth(size, region)
			tc.prepare(s)

			got, err := readSynth(t, s, tc.opts...)
			if err == nil {
				t.Fatalf("Read returned %s with no error", got.Content)
			}
			if got.Content != ContentUnknown {
				t.Errorf("Read returned Content %s alongside its error, want the zero value", got.Content)
			}
			if got.Content == ContentBlank {
				t.Fatal("a failed read reported Blank, which is the defect this package exists to remove")
			}
		})
	}
}

// U-28: the device is all zeros and its tail read fails. This is the case the
// 2026-09-03 incident turned on: everything the prober managed to read was
// empty, and the answer must still be a failure rather than Blank.
func TestAZeroHeadWithAFailingTailIsNotBlank(t *testing.T) {
	s := newSynth(8<<20, MinRegionSize)
	s.failAt = func(off int64) error {
		if off == 0 {
			return nil // the head reads fine, and is all zeros
		}
		return errors.New("input/output error")
	}

	got, err := readSynth(t, s)
	if err == nil {
		t.Fatalf("Read returned %s with no error", got.Content)
	}
	if got.Content == ContentBlank {
		t.Fatal("an all-zero head with an unreadable tail reported Blank")
	}
}

// U-31: a region too small to cover the catalog is refused rather than clamped,
// because a clamped region would report devices blank that a full one recognizes.
func TestTooSmallARegionIsRefused(t *testing.T) {
	s := newSynth(8<<20, 4096)
	got, err := readSynth(t, s)
	if err == nil {
		t.Fatalf("Read returned %s with no error for a %d-byte region", got.Content, 4096)
	}
	if !strings.Contains(err.Error(), "region") {
		t.Errorf("error does not name the region: %v", err)
	}
	if len(s.reads) != 0 {
		t.Errorf("a refused configuration still read the device %d times", len(s.reads))
	}
}

// A prober with no timeout is refused for the same reason: a read that cannot
// time out cannot fail, and a probe that never returns is worse than one that does.
func TestNoTimeoutIsRefused(t *testing.T) {
	s := newSynth(8<<20, MinRegionSize)
	if _, err := readSynth(t, s, WithTimeout(0)); err == nil {
		t.Fatal("a prober with no timeout was accepted")
	}
}

// U-32: the reading never writes. The Reader it is given exposes no way to, so
// this is the interface's guarantee rather than a runtime check, and the test
// records it where a future change to the interface would have to face it.
func TestReaderCannotWrite(t *testing.T) {
	var r Reader = newSynth(1<<20, MinRegionSize)
	if _, isWriter := r.(interface {
		WriteAt([]byte, int64) (int, error)
	}); isWriter {
		t.Fatal("the Reader a prober is handed can write to the device")
	}
}
