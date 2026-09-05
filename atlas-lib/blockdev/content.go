// What a block device carries, read from its own bytes.
//
// This is the evidence an irreversible write rests on. Asking a tool what it
// recognized cannot answer it: blkid reports "no filesystem here" and "this
// device could not be read" with the same exit code, and it reports nothing at
// all for signatures it does not know, so an absence of recognition ends up read
// as an absence of data. That is what formatted a data-bearing volume on
// 2026-09-03.
//
// Specified by operator/docs/designs/design-device-content-detection.md.

package blockdev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Content is what a device was found to carry. The zero value permits nothing,
// so a Reading that was never populated cannot authorize a format.
type Content int

const (
	// ContentUnknown is the zero value and is never returned by Read. A Reading
	// a caller constructed, defaulted, or failed to populate is the one case
	// where a wrong answer is silent, so the zero value is the reading that
	// authorizes nothing.
	ContentUnknown Content = iota

	// ContentBlank means every byte of the probed regions was read successfully
	// and was zero. It is the only reading that permits a format.
	ContentBlank

	// ContentFilesystem means the device carries a filesystem this driver
	// mounts. Reading.Type names it.
	ContentFilesystem

	// ContentStackLayer means the device carries a layer that owns it without
	// being mountable, which today is an LVM physical-volume label.
	ContentStackLayer

	// ContentForeign means the device carries something else: a recognized
	// format this driver does not create, or bytes that match nothing known.
	ContentForeign
)

// String names the content for a log line, an event, and a test failure.
func (c Content) String() string {
	switch c {
	case ContentBlank:
		return "Blank"
	case ContentFilesystem:
		return "Filesystem"
	case ContentStackLayer:
		return "StackLayer"
	case ContentForeign:
		return "Foreign"
	case ContentUnknown:
		return "Unknown"
	default:
		return "Content(" + itoa(int(c)) + ")"
	}
}

// Reading is what one probe of a device found.
type Reading struct {
	Content Content

	// Type names the format found, in the spelling the consuming tool uses: a
	// mount type for a filesystem, the label name for a stack layer, and the
	// format's own name for anything foreign. It is empty for ContentBlank.
	Type string

	// Detail is the human-readable finding for an event, a log line, and the
	// error text of a refusal. It says where each signature was found, so a
	// refusal can be checked by hand.
	Detail string
}

// Reader is bounded access to a block device's bytes.
//
// The context is on the method rather than captured at construction because a
// bounded read is the whole point: a probe that cannot be abandoned is a probe
// that reports nothing when a path is gone, and reporting nothing is what the
// caller must never read as an absence of data.
type Reader interface {
	// ReadAt fills p from off. A short read is an error, not a partial answer.
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	Close() error
}

// Opener opens a device for probing. The default opens it O_DIRECT on the local
// host. The integration harness supplies one that reads a device on another
// machine through a node shell.
type Opener func(ctx context.Context, dev Device) (Reader, error)

// DefaultRegionSize is the size of each probed region, chosen so that the head
// covers every signature a known format writes near the start and the tail
// covers those written near the end. See the design's signature catalog.
const DefaultRegionSize = 1 << 20

// DefaultTimeout bounds one region's read. A degraded device is expected to
// exhaust it, and exhausting it is a read failure rather than a blank reading.
const DefaultTimeout = 30 * time.Second

// MinRegionSize is the smallest region that still covers every signature the
// catalog places at a fixed offset, the furthest of which is the Btrfs
// superblock at 65600. A region below it could report a device blank that a
// larger one would have recognized, so it is refused rather than clamped.
//
// The ZFS labels reach further than this and are the one format whose naming
// degrades in a region smaller than the default. Its devices are still refused,
// because their labels are not zero and the zero rule does not depend on the
// catalog.
const MinRegionSize = 128 << 10

// Prober reads what a block device carries.
type Prober struct {
	open       Opener
	regionSize int64
	timeout    time.Duration

	// cfgErr is a configuration the prober will not read with. It is returned
	// by every Read rather than being clamped away, so a misconfigured prober
	// refuses devices instead of reading too little of them and calling the
	// remainder blank.
	cfgErr error
}

// Option configures a Prober.
type Option func(*Prober)

// WithRegionSize sets the size of each probed region.
func WithRegionSize(bytes int64) Option {
	return func(p *Prober) { p.regionSize = bytes }
}

// WithTimeout bounds one region's read.
func WithTimeout(d time.Duration) Option {
	return func(p *Prober) { p.timeout = d }
}

// NewProberWithOpener returns a Prober reading through open.
func NewProberWithOpener(open Opener, opts ...Option) *Prober {
	p := &Prober{open: open, regionSize: DefaultRegionSize, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(p)
	}
	if p.regionSize < MinRegionSize {
		p.cfgErr = fmt.Errorf(
			"blockdev: a region of %d bytes is below the %d needed to cover every signature the catalog places",
			p.regionSize, MinRegionSize)
	}
	if p.timeout <= 0 {
		p.cfgErr = errors.New("blockdev: a probe needs a positive timeout, so that a stalled read fails rather than hangs")
	}
	return p
}

// Read reports what dev carries. It never writes to the device.
//
// A returned error means the device could not be read, and it is never a
// reading: a caller that treats it as ContentBlank reintroduces the defect this
// package was written to remove.
func (p *Prober) Read(ctx context.Context, dev Device) (Reading, error) {
	if p.cfgErr != nil {
		return Reading{}, p.cfgErr
	}
	regions, err := p.read(ctx, dev)
	if err != nil {
		return Reading{}, err
	}

	if finds := detect(regions); len(finds) > 0 {
		best := choose(finds)
		return Reading{Content: best.content, Type: best.typ, Detail: detailOf(finds)}, nil
	}

	// Nothing recognized. The device is blank only if every byte that was read
	// is zero, which is a positive finding rather than the absence of one: a
	// format nobody listed still writes bytes here, and so does a device whose
	// content this catalog has never heard of.
	if off, nonZero := firstNonZero(regions); nonZero {
		return Reading{
			Content: ContentForeign,
			Type:    "",
			Detail: fmt.Sprintf(
				"no known signature, and the probed regions are not empty: first non-zero byte at %d", off),
		}, nil
	}

	return Reading{Content: ContentBlank, Detail: fmt.Sprintf(
		"the first and last %d bytes were read and are zero", p.regionSize)}, nil
}

// read pulls the two regions off the device. A device smaller than two regions
// is read whole rather than being read twice with an overlap.
func (p *Prober) read(ctx context.Context, dev Device) (regions, error) {
	if dev.SizeBytes == 0 {
		return regions{}, errors.New("blockdev: the device reports no size, so its tail cannot be located")
	}
	size := int64(dev.SizeBytes)

	lbs := int64(dev.LogicalBlockSize)
	if lbs <= 0 {
		lbs = 512
	}

	r, err := p.open(ctx, dev)
	if err != nil {
		return regions{}, fmt.Errorf("blockdev: open %s: %w", dev.Path, err)
	}
	defer func() { _ = r.Close() }()

	out := regions{size: size, lbs: lbs}

	if size <= 2*p.regionSize {
		if out.head, err = p.readAt(ctx, r, 0, size); err != nil {
			return regions{}, err
		}
		out.tailAt = size
		return out, nil
	}

	if out.head, err = p.readAt(ctx, r, 0, p.regionSize); err != nil {
		return regions{}, err
	}
	out.tailAt = size - p.regionSize
	if out.tail, err = p.readAt(ctx, r, out.tailAt, p.regionSize); err != nil {
		return regions{}, err
	}
	return out, nil
}

// readAt reads exactly n bytes at off, bounded by the prober's timeout.
//
// A read that does not return in time is a read failure and is reported as one.
// That does not stop the kernel's outstanding I/O, which no caller can, and the
// distinction that matters is not whether the read is stopped: it is that a
// probe which never answered is reported as a probe that never answered rather
// than as an empty result.
func (p *Prober) readAt(ctx context.Context, r Reader, off, n int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	buf := make([]byte, n)
	got, err := r.ReadAt(ctx, buf, off)
	if err != nil {
		return nil, fmt.Errorf("blockdev: read %d bytes at %d: %w", n, off, err)
	}
	if int64(got) != n {
		return nil, fmt.Errorf("blockdev: short read at %d: %d of %d bytes", off, got, n)
	}
	return buf, nil
}

// firstNonZero reports the absolute offset of the first byte that is not zero.
func firstNonZero(r regions) (int64, bool) {
	if i := bytes.IndexFunc(r.head, nonZero); i >= 0 {
		return int64(i), true
	}
	if i := bytes.IndexFunc(r.tail, nonZero); i >= 0 {
		return r.tailAt + int64(i), true
	}
	return 0, false
}

func nonZero(c rune) bool { return c != 0 }

// detailOf names every signature found, not just the one that decided the
// reading, so a refusal shows an operator the whole device in one message.
func detailOf(finds []find) string {
	parts := make([]string, 0, len(finds))
	for _, f := range finds {
		parts = append(parts, f.detail)
	}
	return strings.Join(parts, ", ")
}

// itoa avoids a strconv import for the one place String needs it.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
