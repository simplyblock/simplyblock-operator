// Package fiemap tells what a byte range of a file is made of: data on the
// device, an unwritten extent, a delayed allocation, or a hole.
//
// All four but the first read back as zeros, and which one it is says where a
// write was lost: never allocated, allocated but never converted, or still in
// the page cache. The map comes from the kernel's FIEMAP ioctl (Read), and the
// classification of a range against it (Classify) is a pure function.
package fiemap

// Kind is what a piece of a range is made of.
type Kind string

const (
	Written   Kind = "written"
	Unwritten Kind = "unwritten"
	Delalloc  Kind = "delalloc"
	Hole      Kind = "hole"
)

// FIEMAP extent flags, as the kernel reports them.
const (
	FlagLast      uint32 = 0x1
	FlagUnknown   uint32 = 0x2
	FlagDelalloc  uint32 = 0x4
	FlagUnwritten uint32 = 0x800
)

// Extent is one entry of a file's extent map, in bytes.
type Extent struct {
	Logical  uint64
	Physical uint64
	Length   uint64
	Flags    uint32
}

// Piece is a contiguous part of a range with one Kind. Physical is the device
// offset of its first byte, and zero for a hole or a delayed allocation.
type Piece struct {
	Offset   uint64 `json:"offset"`
	Length   uint64 `json:"length"`
	Kind     Kind   `json:"kind"`
	Physical uint64 `json:"physical,omitempty"`
}

// Classify lays [offset, offset+length) over extents, which must be sorted by
// Logical and not overlap, and returns its pieces in order.
func Classify(extents []Extent, offset, length uint64) []Piece {
	var out []Piece
	pos, end := offset, offset+length
	for _, e := range extents {
		if pos >= end {
			break
		}
		eEnd := e.Logical + e.Length
		if eEnd <= pos {
			continue
		}
		if e.Logical >= end {
			break
		}
		if e.Logical > pos {
			out = append(out, Piece{Offset: pos, Length: e.Logical - pos, Kind: Hole})
			pos = e.Logical
		}
		stop := min(eEnd, end)
		p := Piece{Offset: pos, Length: stop - pos, Kind: kindOf(e.Flags)}
		if p.Kind != Delalloc {
			p.Physical = e.Physical + (pos - e.Logical)
		}
		out = append(out, p)
		pos = stop
	}
	if pos < end {
		out = append(out, Piece{Offset: pos, Length: end - pos, Kind: Hole})
	}
	return out
}

func kindOf(flags uint32) Kind {
	switch {
	case flags&FlagDelalloc != 0:
		return Delalloc
	case flags&FlagUnwritten != 0:
		return Unwritten
	default:
		return Written
	}
}
