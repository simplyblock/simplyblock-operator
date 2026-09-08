// What the huge-page reader reports, and why it reports it per NUMA node as
// well as per size.
//
// A storage node is pinned to a socket, and huge pages are allocated per NUMA
// node, so a host with the right total and the wrong distribution starts SPDK
// on a node that then cannot get memory. The totals and the per-node breakdown
// are therefore both facts, and the tests state both.

package inventory

import (
	"fmt"
	"testing"
)

// hugePageHost is a host with both page sizes allocated: 4096 2 MiB pages, of
// which some are handed out, and 16 1 GiB pages evenly split across two NUMA
// nodes.
func hugePageHost() fixture {
	const twoMiB = "kernel/mm/hugepages/hugepages-2048kB/"
	const oneGiB = "kernel/mm/hugepages/hugepages-1048576kB/"
	return fixture{files: map[string]string{
		twoMiB + "nr_hugepages":      "4096",
		twoMiB + "free_hugepages":    "3000",
		twoMiB + "resv_hugepages":    "8",
		twoMiB + "surplus_hugepages": "0",
		oneGiB + "nr_hugepages":      "16",
		oneGiB + "free_hugepages":    "16",
		oneGiB + "resv_hugepages":    "0",
		oneGiB + "surplus_hugepages": "0",
		"devices/system/node/node0/hugepages/hugepages-1048576kB/nr_hugepages":   "8",
		"devices/system/node/node0/hugepages/hugepages-1048576kB/free_hugepages": "8",
		"devices/system/node/node1/hugepages/hugepages-1048576kB/nr_hugepages":   "8",
		"devices/system/node/node1/hugepages/hugepages-1048576kB/free_hugepages": "8",
		"devices/system/node/node0/hugepages/hugepages-2048kB/nr_hugepages":      "2048",
		"devices/system/node/node0/hugepages/hugepages-2048kB/free_hugepages":    "1500",
		"devices/system/node/node1/hugepages/hugepages-2048kB/nr_hugepages":      "2048",
		"devices/system/node/node1/hugepages/hugepages-2048kB/free_hugepages":    "1500",
		"meminfo": "MemTotal:       263842560 kB\nHugepagesize:       2048 kB\nHugetlb:        25165824 kB",
	}}
}

func TestReadHugePagesReportsEverySizeItFinds(t *testing.T) {
	root := hugePageHost().write(t)

	pages, err := ReadHugePages(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the huge pages: %v", err)
	}

	if pages.DefaultSizeBytes != 2<<20 {
		t.Errorf("read a default page size of %d, want %d", pages.DefaultSizeBytes, 2<<20)
	}
	if len(pages.Pools) != 2 {
		t.Fatalf("read %d pools, want 2: %+v", len(pages.Pools), pages.Pools)
	}
	// Ascending by size, so a caller reading pages.Pools[0] gets the same pool
	// on every host rather than whatever the directory listing yielded.
	if pages.Pools[0].SizeBytes != 2<<20 || pages.Pools[1].SizeBytes != 1<<30 {
		t.Errorf("read pool sizes %d and %d, want %d and %d ascending",
			pages.Pools[0].SizeBytes, pages.Pools[1].SizeBytes, 2<<20, 1<<30)
	}

	small, ok := pages.Pool(2 << 20)
	if !ok {
		t.Fatal("the 2 MiB pool is missing")
	}
	if small.Total != 4096 || small.Free != 3000 || small.Reserved != 8 || small.Surplus != 0 {
		t.Errorf("read %+v for the 2 MiB pool, want 4096 total, 3000 free, 8 reserved, 0 surplus", small)
	}
	if small.AllocatedBytes() != 4096*(2<<20) {
		t.Errorf("the 2 MiB pool reports %d bytes allocated, want %d",
			small.AllocatedBytes(), 4096*(2<<20))
	}
}

func TestReadHugePagesReportsThePerNodeDistribution(t *testing.T) {
	root := hugePageHost().write(t)

	pages, err := ReadHugePages(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the huge pages: %v", err)
	}

	large, ok := pages.Pool(1 << 30)
	if !ok {
		t.Fatal("the 1 GiB pool is missing")
	}
	if len(large.NUMANodes) != 2 {
		t.Fatalf("read %d NUMA entries for the 1 GiB pool, want 2: %+v", len(large.NUMANodes), large.NUMANodes)
	}
	for i, want := range []NUMAHugePages{{Node: 0, Total: 8, Free: 8}, {Node: 1, Total: 8, Free: 8}} {
		if large.NUMANodes[i] != want {
			t.Errorf("NUMA entry %d is %+v, want %+v", i, large.NUMANodes[i], want)
		}
	}
}

func TestReadHugePagesOrdersNodesByIDAndNotByName(t *testing.T) {
	// The node directory names sort lexicographically, which puts node10
	// before node2. A caller reading Pools[i].NUMANodes[j] as "node j" would
	// be reading the wrong node's free count on any host with enough of them,
	// which is a four-socket machine with sub-NUMA clustering on.
	f := fixture{files: map[string]string{
		"kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages": "44",
		"meminfo": "Hugepagesize:       2048 kB",
	}}
	for node := range 11 {
		dir := fmt.Sprintf("devices/system/node/node%d/hugepages/hugepages-1048576kB/", node)
		f.files[dir+"nr_hugepages"] = "4"
		f.files[dir+"free_hugepages"] = fmt.Sprint(node)
	}

	root := f.write(t)
	pages, err := ReadHugePages(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the huge pages: %v", err)
	}

	pool, ok := pages.Pool(1 << 30)
	if !ok {
		t.Fatal("the 1 GiB pool is missing")
	}
	if len(pool.NUMANodes) != 11 {
		t.Fatalf("read %d NUMA entries, want 11", len(pool.NUMANodes))
	}
	for i, entry := range pool.NUMANodes {
		if entry.Node != i {
			t.Fatalf("entry %d is node %d, want node %d", i, entry.Node, i)
		}
		if entry.Free != uint64(i) {
			t.Errorf("node %d reports %d free, want %d: the entry belongs to "+
				"another node", entry.Node, entry.Free, i)
		}
	}
}

func TestReadHugePagesTotalsEverySize(t *testing.T) {
	root := hugePageHost().write(t)

	pages, err := ReadHugePages(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the huge pages: %v", err)
	}

	want := uint64(4096*(2<<20) + 16*(1<<30))
	if got := pages.AllocatedBytes(); got != want {
		t.Errorf("read %d bytes allocated in total, want %d", got, want)
	}
}

func TestReadHugePagesReportsNoneRatherThanFailingOnAHostWithout(t *testing.T) {
	// A kernel built without hugetlbfs exports no directory at all. Nothing is
	// wrong with such a host: it has no huge pages, which is a finding a
	// discovery run reports rather than an error that hides the rest of the
	// inventory behind it.
	root := fixture{files: map[string]string{"meminfo": "MemTotal:  1024 kB"}}.write(t)

	pages, err := ReadHugePages(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the huge pages of a host without any: %v", err)
	}
	if len(pages.Pools) != 0 {
		t.Errorf("read %d pools on a host with no hugetlbfs, want none", len(pages.Pools))
	}
	if pages.AllocatedBytes() != 0 {
		t.Errorf("read %d bytes allocated on a host with no hugetlbfs, want 0", pages.AllocatedBytes())
	}
}

func TestReadHugePagesIgnoresADirectoryItCannotName(t *testing.T) {
	f := hugePageHost()
	f.files["kernel/mm/hugepages/hugepages-notasize/nr_hugepages"] = "1"

	root := f.write(t)
	pages, err := ReadHugePages(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the huge pages: %v", err)
	}
	if len(pages.Pools) != 2 {
		t.Errorf("read %d pools, want 2: a directory whose size cannot be parsed "+
			"is not a pool, and must not become one of unknown size", len(pages.Pools))
	}
}
