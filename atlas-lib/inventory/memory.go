// How much memory the machine has, and how much of it is actually available.
//
// Three numbers, because two of them are routinely confused and the third is
// invisible in both. Free is memory nothing holds at all. Available is the
// kernel's own estimate of what a new process could get, which on a busy host
// is far larger, because most of what is not free is page cache the kernel
// hands back on demand: 4 GiB free and 200 GiB available is an ordinary
// reading, and sizing a storage node against the first would refuse a machine
// with plenty.
//
// The third is what huge pages have taken. That memory leaves the general pool
// when it is reserved, not when it is used, so it appears in neither free nor
// available and has to be counted on its own. It is the same memory
// [HugePages] describes per pool; this is the total, from the machine's point
// of view rather than the pool's.
//
// The per-node split is here for the reason everything else in this package
// carries one: a storage node pinned to a socket draws its memory from that
// socket, and a host with 256 GiB whose chosen node has 4 GiB free cannot start
// one there.

package inventory

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Memory is the machine's memory, in bytes throughout.
//
// The kernel reports these in kibibytes and this converts, because every other
// size in the inventory is in bytes and one unit per value is what keeps a
// caller from multiplying twice.
type Memory struct {
	// TotalBytes is all usable RAM, which is what a machine is bought by.
	TotalBytes uint64

	// FreeBytes is memory nothing holds. It is the smaller and less useful of
	// the two availability readings, and it is reported because a caller
	// looking for it should find it here rather than reach for AvailableBytes
	// thinking it is this.
	FreeBytes uint64

	// AvailableBytes is the kernel's estimate of what a new process could get
	// without swapping, which is free plus the page cache it would reclaim. It
	// is the number to size against.
	AvailableBytes uint64

	// HugePagesBytes is memory reserved as huge pages, which is out of the
	// general pool whether or not anything has faulted the pages in. It is
	// meminfo's Hugetlb.
	//
	// It is not HugePagePool.Reserved, which is a different thing with a
	// similar name: that is pages promised to a mapping that has not touched
	// them yet, within a pool.
	HugePagesBytes uint64

	// SwapTotalBytes and SwapFreeBytes describe the swap the host has. A
	// storage node host with swap in use is one whose memory is already
	// oversubscribed.
	SwapTotalBytes, SwapFreeBytes uint64

	// NUMANodes is the same reading per memory node, ascending, and is empty on
	// a kernel that exports no node directories.
	NUMANodes []NUMAMemory
}

// UsedBytes is total less available: the memory a new process could not get.
func (m Memory) UsedBytes() uint64 {
	if m.AvailableBytes > m.TotalBytes {
		return 0
	}
	return m.TotalBytes - m.AvailableBytes
}

// NUMAMemory is one memory node's share.
//
// It carries no availability estimate, because the kernel does not compute one
// per node: node meminfo reports total, free, and used, and an available figure
// would have to be invented.
type NUMAMemory struct {
	// Node is the memory node's id, as in devices/system/node/nodeN.
	Node int

	// TotalBytes and FreeBytes are that node's own.
	TotalBytes, FreeBytes uint64
}

// meminfoScale is the unit /proc/meminfo and the per-node meminfo report in.
const meminfoScale = 1024

// ReadMemory reads the machine's memory.
//
// An unreadable meminfo is a failure rather than a zero reading. A host whose
// memory could not be read is not a host with no memory, and reporting zero
// would let a caller size a storage node against a machine nothing can run on.
func ReadMemory(cfg Config) (Memory, error) {
	path := cfg.procPath("meminfo")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Memory{}, fmt.Errorf("read %s: %w", path, err)
	}

	fields := meminfoFields(string(raw), "")
	memory := Memory{
		TotalBytes:     fields["MemTotal"],
		FreeBytes:      fields["MemFree"],
		AvailableBytes: fields["MemAvailable"],
		HugePagesBytes: fields["Hugetlb"],
		SwapTotalBytes: fields["SwapTotal"],
		SwapFreeBytes:  fields["SwapFree"],
	}
	if memory.TotalBytes == 0 {
		return Memory{}, fmt.Errorf("%s reports no MemTotal, so the machine's memory is unknown", path)
	}

	memory.NUMANodes = readNUMAMemory(cfg)
	return memory, nil
}

// readNUMAMemory reads each memory node's own meminfo.
//
// A node's file prefixes every line with its own node number, so the keys are
// read with that prefix stripped rather than by counting fields: it is the only
// difference from the machine-wide file, and stripping it lets one parser read
// both.
func readNUMAMemory(cfg Config) []NUMAMemory {
	base := cfg.sysfsPath(nodeDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}

	var nodes []NUMAMemory
	for _, entry := range entries {
		id, isNode := nodeID(entry.Name())
		if !isNode {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(base, entry.Name(), "meminfo"))
		if err != nil {
			continue
		}

		fields := meminfoFields(string(raw), fmt.Sprintf("Node %d ", id))
		nodes = append(nodes, NUMAMemory{
			Node:       id,
			TotalBytes: fields["MemTotal"],
			FreeBytes:  fields["MemFree"],
		})
	}

	// Ascending by id and not by directory name, which puts node10 before
	// node2 and would have a caller reading one node's memory as another's.
	slices.SortFunc(nodes, func(a, b NUMAMemory) int { return cmp.Compare(a.Node, b.Node) })
	return nodes
}

// meminfoFields parses a meminfo file into bytes by key, dropping the prefix a
// per-node file carries.
//
// A line pairs a key with a value and a unit, and the unit is always kB
// whatever the kernel writes: the field is documented in kibibytes and none
// emits anything else. A line carrying no unit is read as a bare count, which
// is what the huge-page counters are.
func meminfoFields(text, prefix string) map[string]uint64 {
	fields := map[string]uint64{}
	for line := range strings.Lines(text) {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !found {
			continue
		}

		key, value, split := strings.Cut(rest, ":")
		if !split {
			continue
		}
		parts := strings.Fields(value)
		if len(parts) == 0 {
			continue
		}
		amount, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		if len(parts) > 1 && strings.EqualFold(parts[1], "kB") {
			amount *= meminfoScale
		}
		fields[strings.TrimSpace(key)] = amount
	}
	return fields
}
