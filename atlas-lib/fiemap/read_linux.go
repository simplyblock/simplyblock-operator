//go:build linux

package fiemap

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fsIocFiemap is FS_IOC_FIEMAP, _IOWR('f', 11, struct fiemap).
const fsIocFiemap = 0xC020660B

// batch is how many extents one ioctl returns.
const batch = 128

// fiemapHeader and fiemapExtent are struct fiemap and struct fiemap_extent
// from linux/fiemap.h.
type fiemapHeader struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
}

type fiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	Reserved64 [2]uint64
	Flags      uint32
	Reserved   [3]uint32
}

type fiemapRequest struct {
	fiemapHeader
	Extents [batch]fiemapExtent
}

// Read classifies [offset, offset+length) of the file at path. It does not
// ask the kernel to flush first, so a range still in the page cache shows as
// delalloc rather than being allocated by the act of looking.
func Read(path string, offset, length uint64) ([]Piece, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ReadFile(f, offset, length)
}

// ReadFile is Read on a file the caller has already opened and checked.
func ReadFile(f *os.File, offset, length uint64) ([]Piece, error) {
	path := f.Name()
	var extents []Extent
	pos, end := offset, offset+length
	for pos < end {
		req := fiemapRequest{fiemapHeader: fiemapHeader{Start: pos, Length: end - pos, ExtentCount: batch}}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap,
			uintptr(unsafe.Pointer(&req))); errno != 0 {
			return nil, fmt.Errorf("fiemap %s: %w", path, errno)
		}
		n := int(req.MappedExtents)
		if n == 0 {
			break
		}
		last := false
		for _, e := range req.Extents[:n] {
			extents = append(extents, Extent{Logical: e.Logical, Physical: e.Physical, Length: e.Length, Flags: e.Flags})
			last = last || e.Flags&FlagLast != 0
		}
		next := req.Extents[n-1].Logical + req.Extents[n-1].Length
		if last || next <= pos {
			break
		}
		pos = next
	}
	return Classify(extents, offset, length), nil
}
