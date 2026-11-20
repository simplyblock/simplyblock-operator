// What an NVMe namespace promises about a write a power failure interrupts,
// decoded from the Identify data the LBA formats come from.
//
// The kernel publishes the same promise as queue/atomic_write_unit_* only from
// 6.11 on, and it reads less of it: it ignores the controller's AWUPF and gives
// up on a namespace whose atomic boundaries are offset. Reading the fields here
// answers on every kernel and keeps each one, so the caller decides what to
// trust.

package nvme

import "encoding/binary"

// Power-fail atomicity fields of the Identify Namespace data structure (NVM
// Command Set), little-endian, all 0's based counts of logical blocks.
//
//	offset  size  field    meaning
//	------  ----  -------  ------------------------------------------------
//	    24     1  NSFEAT   bit 1 (NSABP): the namespace states its own values
//	    36     2  NAWUPF   namespace atomic write unit, power fail
//	    42     2  NABO     namespace atomic boundary offset
//	    44     2  NABSPF   namespace atomic boundary size, power fail
const (
	nsfeatOffset  = 24
	nsfeatAtomics = 1 << 1
	nawupfOffset  = 36
	naboOffset    = 42
	nabspfOffset  = 44
)

// PowerFailAtomicity is the largest write a namespace completes whole across a
// power failure, and the boundaries such a write must not cross. Every size is
// a count of the namespace's logical blocks, in the format it had when read.
type PowerFailAtomicity struct {
	// NamespaceSpecific reports NSFEAT bit 1: the namespace states its own
	// atomic write unit, which then replaces the controller's.
	NamespaceSpecific bool

	// NamespaceBlocks is NAWUPF plus one, and zero unless NamespaceSpecific.
	NamespaceBlocks uint32

	// ControllerBlocks is the controller's AWUPF plus one, and zero when the
	// controller was not read.
	ControllerBlocks uint32

	// BoundaryBlocks is NABSPF plus one, and zero when the namespace has no
	// atomic boundaries. BoundaryOffset is NABO, where the first one falls.
	BoundaryBlocks uint32
	BoundaryOffset uint32
}

// Blocks is the atomic write unit that applies: the namespace's own where it
// states one, the controller's otherwise, as the specification orders them.
func (a PowerFailAtomicity) Blocks() uint32 {
	if a.NamespaceSpecific {
		return a.NamespaceBlocks
	}
	return a.ControllerBlocks
}

// AtomicAt reports whether a write of size bytes, aligned to size, completes
// whole across a power failure on blocks of blockSize bytes.
//
// One logical block always does. A larger write must fit the atomic write unit
// and must not cross an atomic boundary, which an aligned write avoids only
// when the boundaries are spaced and offset in multiples of its size.
func (a PowerFailAtomicity) AtomicAt(size, blockSize uint32) bool {
	if blockSize == 0 || size%blockSize != 0 {
		return false
	}
	blocks := size / blockSize
	if blocks <= 1 {
		return blocks == 1
	}
	if a.Blocks() < blocks {
		return false
	}
	if a.BoundaryBlocks == 0 {
		return true
	}
	return a.BoundaryBlocks%blocks == 0 && a.BoundaryOffset%blocks == 0
}

// namespaceAtomicityFromIdentify decodes the namespace's own fields. The
// controller's AWUPF comes from a different command and is added by the caller.
func namespaceAtomicityFromIdentify(buf []byte) PowerFailAtomicity {
	var a PowerFailAtomicity
	if len(buf) < nabspfOffset+2 {
		return a
	}
	if buf[nsfeatOffset]&nsfeatAtomics != 0 {
		a.NamespaceSpecific = true
		a.NamespaceBlocks = uint32(binary.LittleEndian.Uint16(buf[nawupfOffset:])) + 1
	}
	// The kernel reads the boundary only under NSFEAT bit 1, and the offset
	// regardless. Both are zero where a controller leaves them reserved.
	if spf := binary.LittleEndian.Uint16(buf[nabspfOffset:]); spf != 0 {
		a.BoundaryBlocks = uint32(spf) + 1
	}
	a.BoundaryOffset = uint32(binary.LittleEndian.Uint16(buf[naboOffset:]))
	return a
}
