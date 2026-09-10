// What huge-page memory a host has already set aside, per page size and per
// NUMA node.
//
// SPDK does not allocate huge pages, it consumes them, so a storage node either
// finds them reserved before it starts or does not start. The per-node
// breakdown is here for the same reason the totals are: a node pinned to one
// socket draws from that socket's pages, so a host with the right total and the
// wrong distribution reports enough memory and then fails to get any.
//
// The kernel names its directories in kibibytes and this package reports bytes,
// because every other size in the inventory is in bytes and one unit per value
// is what keeps a caller from multiplying twice.

package inventory

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/simplyblock/atlas/internal/sysfs"
)

// HugePages is a host's huge-page allocation.
type HugePages struct {
	// DefaultSizeBytes is the size a mount or an allocation that names none
	// gets, read from meminfo's Hugepagesize. It is zero on a kernel without
	// hugetlbfs.
	DefaultSizeBytes uint64

	// Pools is one entry per page size the kernel supports, ascending by size,
	// so that two readings of one host are comparable entry by entry.
	Pools []HugePagePool
}

// HugePagePool is one page size's allocation.
//
// The counts are pages and not bytes, matching the attributes they are read
// from; AllocatedBytes is the multiplication, done once here rather than at
// every call site.
type HugePagePool struct {
	// SizeBytes is the size of one page in this pool, 2 MiB or 1 GiB on x86.
	SizeBytes uint64

	// Total is nr_hugepages: how many pages are set aside for this size.
	Total uint64

	// Free is free_hugepages: how many of them nothing has taken. Free pages
	// include the reserved ones, which is the kernel's own accounting and the
	// reason both fields are reported rather than one difference.
	Free uint64

	// Reserved is resv_hugepages: pages promised to a mapping that has not
	// faulted them in yet.
	Reserved uint64

	// Surplus is surplus_hugepages: pages allocated above Total out of ordinary
	// memory, which the kernel may take back. A pool leaning on surplus is not
	// a pool a storage node should be sized against.
	Surplus uint64

	// NUMANodes is the same allocation broken down per memory node, ascending
	// by node, and is empty on a host that exports no per-node accounting.
	NUMANodes []NUMAHugePages
}

// AllocatedBytes is how much memory this pool holds.
func (p HugePagePool) AllocatedBytes() uint64 {
	return p.Total * p.SizeBytes
}

// FreeBytes is how much of this pool nothing has taken.
func (p HugePagePool) FreeBytes() uint64 {
	return p.Free * p.SizeBytes
}

// NUMAHugePages is one memory node's share of one pool.
type NUMAHugePages struct {
	// Node is the memory node's id, as in devices/system/node/nodeN.
	Node int

	// Total and Free are that node's nr_hugepages and free_hugepages.
	Total, Free uint64
}

// Pool returns the pool for a page size, and reports whether the host has one.
func (h HugePages) Pool(sizeBytes uint64) (HugePagePool, bool) {
	for _, p := range h.Pools {
		if p.SizeBytes == sizeBytes {
			return p, true
		}
	}
	return HugePagePool{}, false
}

// AllocatedBytes is how much huge-page memory the host holds across every size.
func (h HugePages) AllocatedBytes() uint64 {
	var total uint64
	for _, p := range h.Pools {
		total += p.AllocatedBytes()
	}
	return total
}

// hugePagesDir is where the kernel exports the global pools, and the per-node
// directories repeat the same layout under each node.
const (
	hugePagesDir   = "kernel/mm/hugepages"
	nodeDir        = "devices/system/node"
	hugePagePrefix = "hugepages-"
	hugePageSuffix = "kB"
)

// ReadHugePages reads a host's huge-page allocation.
//
// A kernel built without hugetlbfs exports no directory, and that is a host
// with no huge pages rather than a failure: nothing is wrong with it, and
// failing would hide the rest of the inventory behind a fact a caller can act
// on directly.
func ReadHugePages(cfg Config) (HugePages, error) {
	var pages HugePages
	pages.DefaultSizeBytes = readDefaultHugePageSize(cfg)

	base := cfg.sysfsPath(hugePagesDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return pages, nil
		}
		return pages, fmt.Errorf("list %s: %w", base, err)
	}

	perNode, err := readNodeHugePages(cfg)
	if err != nil {
		return pages, err
	}

	for _, entry := range entries {
		size, ok := parseHugePageDirName(entry.Name())
		if !ok {
			// A directory whose size cannot be read is not a pool. Reporting it
			// with a size of zero would let a caller add it to a total and be
			// wrong about how much memory the host holds.
			continue
		}
		dir := filepath.Join(base, entry.Name())
		pages.Pools = append(pages.Pools, HugePagePool{
			SizeBytes: size,
			Total:     sysfs.Uint64(dir, "nr_hugepages"),
			Free:      sysfs.Uint64(dir, "free_hugepages"),
			Reserved:  sysfs.Uint64(dir, "resv_hugepages"),
			Surplus:   sysfs.Uint64(dir, "surplus_hugepages"),
			NUMANodes: perNode[size],
		})
	}

	slices.SortFunc(pages.Pools, func(a, b HugePagePool) int {
		return cmp.Compare(a.SizeBytes, b.SizeBytes)
	})
	return pages, nil
}

// readNodeHugePages collects the per-node breakdown, keyed by page size.
//
// It walks the nodes rather than the sizes because the node directories are
// what may be missing: a kernel without CONFIG_NUMA exports the global pools
// and no nodes at all.
func readNodeHugePages(cfg Config) (map[uint64][]NUMAHugePages, error) {
	base := cfg.sysfsPath(nodeDir)
	nodes, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", base, err)
	}

	perSize := map[uint64][]NUMAHugePages{}

	for _, node := range nodes {
		id, isNode := nodeID(node.Name())
		if !isNode {
			continue
		}

		nodeHugePages := filepath.Join(base, node.Name(), "hugepages")
		sizes, err := os.ReadDir(nodeHugePages)
		if err != nil {
			continue
		}
		for _, entry := range sizes {
			size, ok := parseHugePageDirName(entry.Name())
			if !ok {
				continue
			}
			dir := filepath.Join(nodeHugePages, entry.Name())
			perSize[size] = append(perSize[size], NUMAHugePages{
				Node:  id,
				Total: sysfs.Uint64(dir, "nr_hugepages"),
				Free:  sysfs.Uint64(dir, "free_hugepages"),
			})
		}
	}

	// Ascending by node id, and by the id rather than by the directory name:
	// the names sort lexicographically, which puts node10 before node2 on a
	// host with enough of them.
	for size := range perSize {
		slices.SortFunc(perSize[size], func(a, b NUMAHugePages) int {
			return cmp.Compare(a.Node, b.Node)
		})
	}
	return perSize, nil
}

// parseHugePageDirName turns "hugepages-2048kB" into 2 MiB in bytes, and
// reports whether the name was one it could read.
func parseHugePageDirName(name string) (uint64, bool) {
	rest, ok := strings.CutPrefix(name, hugePagePrefix)
	if !ok {
		return 0, false
	}
	rest, ok = strings.CutSuffix(rest, hugePageSuffix)
	if !ok {
		return 0, false
	}
	kib, err := strconv.ParseUint(rest, 10, 64)
	if err != nil || kib == 0 {
		return 0, false
	}
	return kib * 1024, true
}

// readDefaultHugePageSize reads meminfo's Hugepagesize, which is the size an
// allocation that names none gets. A host without hugetlbfs has no such line,
// and zero is then the honest answer.
func readDefaultHugePageSize(cfg Config) uint64 {
	raw, err := os.ReadFile(cfg.procPath("meminfo"))
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(raw)) {
		rest, found := strings.CutPrefix(line, "Hugepagesize:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kib * 1024
	}
	return 0
}
