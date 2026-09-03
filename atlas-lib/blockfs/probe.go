// The probe itself: the states it can conclude, the Prober seam consumers
// depend on, and the local implementation that reads a real device. Kept apart
// from signature.go so the decision procedure reads in one piece, without the
// magic-number table in the way.
package blockfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/simplyblock/atlas/errs/deferrers"
)

// State is what a probe concluded about a device.
type State string

const (
	// StateFormatted: the device holds a filesystem that can be mounted as it
	// is. Formatting it would destroy data.
	StateFormatted State = "Formatted"

	// StateForeign: the device holds a recognized signature that is not a
	// mountable filesystem — an encrypted container, an LVM physical volume, a
	// swap area, or a partition table. It is somebody's data even though it
	// cannot be mounted, so formatting it would still destroy something.
	StateForeign State = "Foreign"

	// StateBlank: the device answered the read, and every byte of it is zero.
	// This is the only state from which formatting is safe.
	StateBlank State = "Blank"

	// StateUnknown: the device answered the read and carries no signature this
	// package knows, but it is not all zeros either. Nothing identifies the
	// content, and nothing rules out that it matters.
	StateUnknown State = "Unknown"

	// StateUnreadable: the device did not answer the read, so nothing at all
	// can be concluded about what it holds. Never treat this as blank — it is
	// what a volume behind a lost or timed-out NVMe-oF path looks like.
	StateUnreadable State = "Unreadable"
)

// Result is a probe's conclusion.
type Result struct {
	State State
	// Signature names what was matched, for StateFormatted and StateForeign.
	// It is a filesystem family ("ext" covers ext2, ext3, and ext4), not an
	// exact revision, and is empty in every other state.
	Signature string
	// Err is why the device could not be read. Set only for StateUnreadable.
	Err error
}

// Prober reads a block device and reports whether it already holds data.
// Consumers depend on the interface so their tests need no block device.
type Prober interface {
	// Probe never returns an error beside its Result: a device that cannot be
	// read is a conclusion the caller has to act on (StateUnreadable), not a
	// failure of the call, and collapsing the two is the mistake this package
	// exists to prevent.
	Probe(ctx context.Context, device string) Result
}

// ReaderAtCloser is the part of a device a probe uses.
type ReaderAtCloser interface {
	io.ReaderAt
	io.Closer
}

// Opener opens a device for probing. It is a seam: a test supplies one that
// fails the way a dead path does, which no temporary file can imitate.
type Opener func(device string) (ReaderAtCloser, error)

// defaultProbeTimeout bounds a single probe. It sits under the 30 seconds
// nvme_core.io_timeout gives an NVMe command by default, so a probe against a
// stalled path resolves as unreadable rather than holding a NodeStageVolume
// open until the kernel gives up.
const defaultProbeTimeout = 20 * time.Second

// DeviceProber probes a real block device.
type DeviceProber struct {
	open    Opener
	timeout time.Duration
}

// Option configures a DeviceProber.
type Option func(*DeviceProber)

// WithOpener replaces how a device is opened.
func WithOpener(open Opener) Option {
	return func(p *DeviceProber) { p.open = open }
}

// WithTimeout replaces how long a single probe may take.
func WithTimeout(timeout time.Duration) Option {
	return func(p *DeviceProber) { p.timeout = timeout }
}

// NewDeviceProber returns a Prober reading local block devices.
func NewDeviceProber(opts ...Option) *DeviceProber {
	p := &DeviceProber{
		open:    func(device string) (ReaderAtCloser, error) { return os.Open(device) },
		timeout: defaultProbeTimeout,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

var _ Prober = (*DeviceProber)(nil)

// Probe reads the start of device and classifies what it found.
func (p *DeviceProber) Probe(ctx context.Context, device string) Result {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	f, err := p.open(device)
	if err != nil {
		return Result{State: StateUnreadable, Err: fmt.Errorf("open %s: %w", device, err)}
	}

	type readResult struct {
		data []byte
		err  error
	}
	done := make(chan readResult, 1)

	// The goroutine owns the file and the buffer outright, and nothing here
	// touches either afterward. A read against a device whose paths are gone
	// returns no sooner for being abandoned, so on a timeout this call leaves
	// the read running and the goroutine ends on its own once the kernel
	// completes or fails the I/O.
	go func() {
		defer deferrers.Close(f)

		buf := make([]byte, probeLength)
		n, readErr := f.ReadAt(buf, 0)
		if n < 0 {
			n = 0
		}
		done <- readResult{data: buf[:n], err: readErr}
	}()

	select {
	case <-ctx.Done():
		return Result{State: StateUnreadable, Err: fmt.Errorf("read %s: %w", device, ctx.Err())}
	case r := <-done:
		return classify(r.data, r.err)
	}
}

// classify turns the bytes a probe managed to read into a conclusion.
//
// A read that failed can still be conclusive. A signature found in the part
// that did arrive proves the device holds data whatever became of the rest,
// and resolving a partial read that way errs toward keeping the data. Absent a
// signature, a failed read concludes nothing: neither "blank" nor "unknown"
// may be inferred from bytes that never came.
func classify(data []byte, readErr error) Result {
	if sig, ok := match(data); ok {
		if sig.mountable {
			return Result{State: StateFormatted, Signature: sig.name}
		}
		return Result{State: StateForeign, Signature: sig.name}
	}

	// io.EOF is how ReadAt reports a device shorter than the probe window,
	// which is a small device rather than a failure. Every signature above
	// still falls within the bytes that arrived, or beyond the device's end.
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return Result{State: StateUnreadable, Err: readErr}
	}
	if len(data) == 0 {
		return Result{State: StateUnreadable, Err: errors.New("device returned no bytes")}
	}

	if isZero(data) {
		return Result{State: StateBlank}
	}
	return Result{State: StateUnknown}
}
