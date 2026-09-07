//go:build linux

// Reading a block device on the host this process runs on, and resolving what
// the kernel says about it.
//
// Reads bypass the page cache. A device that was read before its fabric broke
// can serve its old contents, or zeros for a region never faulted in, out of
// cache afterward, and a probe answering from cache is answering about the past.

package blockdev

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// NewProber returns a Prober reading local devices, bypassing the page cache.
func NewProber(opts ...Option) *Prober { return NewProberWithOpener(OpenLocal, opts...) }

// OpenLocal opens dev for probing on this host, read-only and with O_DIRECT.
//
// A kernel or filesystem that refuses O_DIRECT falls back to a buffered read
// preceded by a cache drop, and the reader records that it did, so the
// difference is visible rather than silent.
func OpenLocal(_ context.Context, dev Device) (Reader, error) {
	align := int(dev.LogicalBlockSize)
	if align <= 0 {
		align = 512
	}

	f, err := os.OpenFile(dev.Path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err == nil {
		return &localReader{f: f, align: align, direct: true}, nil
	}

	// O_DIRECT is refused by some kernels and by most non-block-device files.
	// The buffered path drops the cache first so a stale page is not what the
	// probe reads, which is the property O_DIRECT was there for.
	f, ferr := os.OpenFile(dev.Path, os.O_RDONLY, 0)
	if ferr != nil {
		return nil, ferr
	}
	dropCache(f)
	return &localReader{f: f, align: align, direct: false}, nil
}

// dropCache asks the kernel to forget what it has cached for this device. It is
// best effort: the ioctl is not available for every device, and failing it
// leaves the read no worse off than a probe that never tried.
func dropCache(f *os.File) {
	const blkflsbuf = 0x1261
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, f.Fd(), blkflsbuf, 0)
}

// localReader reads one open device.
type localReader struct {
	f      *os.File
	align  int
	direct bool
}

// Degraded reports whether this reader fell back from O_DIRECT, so a caller can
// count the reads whose freshness the kernel did not guarantee.
func (r *localReader) Degraded() bool { return !r.direct }

func (r *localReader) Close() error { return r.f.Close() }

// ReadAt fills p from off, bounded by ctx.
//
// The read runs on its own goroutine and ctx is waited on beside it, because a
// pread against a device whose path is gone blocks in the kernel and no caller
// can interrupt it. The goroutine outlives the call in that case, until the
// kernel's own timeout returns. What matters is not that the read stopped: it is
// that the caller is told the probe did not answer, rather than being handed an
// empty result it would read as an empty device.
func (r *localReader) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)

	go func() {
		n, err := r.readAt(p, off)
		done <- result{n, err}
	}()

	select {
	case res := <-done:
		return res.n, res.err
	case <-ctx.Done():
		return 0, fmt.Errorf("read of %d bytes at %d did not return: %w", len(p), off, ctx.Err())
	}
}

// readAt does the read itself, bouncing through an aligned buffer when the file
// was opened O_DIRECT, whose buffer, offset, and length all have to be aligned
// to the device's logical block size.
func (r *localReader) readAt(p []byte, off int64) (int, error) {
	if !r.direct {
		return readFull(r.f, p, off)
	}
	if off%int64(r.align) != 0 || len(p)%r.align != 0 {
		return 0, fmt.Errorf(
			"a direct read has to be aligned to %d bytes, and this one is %d bytes at %d",
			r.align, len(p), off)
	}
	buf := alignedBuffer(len(p), r.align)
	n, err := readFull(r.f, buf, off)
	copy(p, buf[:n])
	return n, err
}

// readFull reads until p is full, since one pread may return less than asked
// for without that being a failure.
func readFull(f *os.File, p []byte, off int64) (int, error) {
	total := 0
	for total < len(p) {
		n, err := f.ReadAt(p[total:], off+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
	}
	return total, nil
}

// alignedBuffer returns a slice of n bytes whose first byte sits on an align
// boundary, which is what O_DIRECT requires and what make cannot promise.
func alignedBuffer(n, align int) []byte {
	buf := make([]byte, n+align)
	off := int(uintptr(unsafe.Pointer(&buf[0])) % uintptr(align))
	if off != 0 {
		off = align - off
	}
	return buf[off : off+n]
}

// ResolveDevice reads what the kernel says about the block device at path.
//
// This is the resolver the volume stack design leaves room for beside Device:
// the type is a value, and this is the one place that fills it in from the host,
// so a caller needing a device's size and block size does not derive them at
// four call sites.
func ResolveDevice(path string) (Device, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Device{}, fmt.Errorf("resolve %s: %w", path, err)
	}

	f, err := os.OpenFile(resolved, os.O_RDONLY, 0)
	if err != nil {
		return Device{}, fmt.Errorf("open %s: %w", resolved, err)
	}
	defer func() { _ = f.Close() }()

	dev := Device{Path: resolved, Name: filepath.Base(resolved)}

	info, err := f.Stat()
	if err != nil {
		return Device{}, fmt.Errorf("stat %s: %w", resolved, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		rdev := uint64(st.Rdev) //nolint:unconvert // Rdev is uint64 on linux/amd64 and uint32 elsewhere
		dev.Major, dev.Minor = unix.Major(rdev), unix.Minor(rdev)
	}

	size, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKGETSIZE64)
	if err != nil {
		return Device{}, fmt.Errorf("read the size of %s: %w", resolved, err)
	}
	dev.SizeBytes = uint64(size)

	if lbs, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKSSZGET); err == nil {
		dev.LogicalBlockSize = uint32(lbs)
	}
	if pbs, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKPBSZGET); err == nil {
		dev.PhysicalBlockSize = uint32(pbs)
	}
	if ro, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKROGET); err == nil {
		dev.ReadOnly = ro != 0
	}

	if dev.SizeBytes == 0 {
		return Device{}, fmt.Errorf("%s reports a size of zero, so it cannot be probed", resolved)
	}
	return dev, nil
}
