// What the memory reader reports, and the three numbers that are easy to
// confuse.
//
// Free and available are not the same and the difference is large: a host with
// 250 GiB of RAM and 4 GiB free may have 200 GiB available, because the rest is
// page cache the kernel gives back on demand. Sizing a storage node against
// free would refuse a machine that has plenty.
//
// The third is what huge pages have taken. That memory is gone from the general
// pool whether or not anything is using the pages, so it is counted apart from
// both.

package inventory

import "testing"

// memoryHost is a two-socket machine with 256 GiB, of which 12 GiB is reserved
// as huge pages, and swap on top.
func memoryHost() fixture {
	return fixture{files: map[string]string{
		"meminfo": "MemTotal:       263842560 kB\n" +
			"MemFree:         4194304 kB\n" +
			"MemAvailable:  209715200 kB\n" +
			"Buffers:          262144 kB\n" +
			"Cached:        201326592 kB\n" +
			"SwapTotal:       8388608 kB\n" +
			"SwapFree:        8388608 kB\n" +
			"Hugepagesize:       2048 kB\n" +
			"Hugetlb:        12582912 kB",
		"devices/system/node/node0/meminfo": "Node 0 MemTotal:       131921280 kB\n" +
			"Node 0 MemFree:         2097152 kB\n" +
			"Node 0 MemUsed:       129824128 kB",
		"devices/system/node/node1/meminfo": "Node 1 MemTotal:       131921280 kB\n" +
			"Node 1 MemFree:         2097152 kB\n" +
			"Node 1 MemUsed:       129824128 kB",
	}}
}

func TestReadMemoryKeepsFreeAndAvailableApart(t *testing.T) {
	root := memoryHost().write(t)

	memory, err := ReadMemory(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the memory: %v", err)
	}

	if memory.TotalBytes != 263842560*1024 {
		t.Errorf("read %d bytes total, want %d", memory.TotalBytes, uint64(263842560)*1024)
	}
	if memory.FreeBytes != 4194304*1024 {
		t.Errorf("read %d bytes free, want %d", memory.FreeBytes, uint64(4194304)*1024)
	}
	if memory.AvailableBytes != 209715200*1024 {
		t.Errorf("read %d bytes available, want %d", memory.AvailableBytes, uint64(209715200)*1024)
	}
	// The whole reason both are reported: on this host free is 4 GiB and
	// available is 200 GiB, and a caller that sized against the first would
	// refuse a machine with plenty.
	if memory.AvailableBytes <= memory.FreeBytes {
		t.Error("available is not larger than free on a host with a page cache, " +
			"so one of the two readings is the other")
	}
}

func TestReadMemoryCountsWhatHugePagesTook(t *testing.T) {
	// Memory in huge pages is out of the general pool whether or not anything
	// has faulted the pages in, so it is neither free nor available and is
	// counted on its own.
	root := memoryHost().write(t)

	memory, err := ReadMemory(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the memory: %v", err)
	}

	if memory.HugePagesBytes != 12582912*1024 {
		t.Errorf("read %d bytes in huge pages, want %d",
			memory.HugePagesBytes, uint64(12582912)*1024)
	}
	if memory.SwapTotalBytes != 8388608*1024 || memory.SwapFreeBytes != 8388608*1024 {
		t.Errorf("read swap %d of %d", memory.SwapFreeBytes, memory.SwapTotalBytes)
	}
}

func TestReadMemoryReportsThePerNodeSplit(t *testing.T) {
	// A storage node pinned to a socket draws its memory from that socket, so
	// the total is not enough: a host with 256 GiB where one node has 4 GiB
	// free cannot start a node there.
	root := memoryHost().write(t)

	memory, err := ReadMemory(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the memory: %v", err)
	}

	if len(memory.NUMANodes) != 2 {
		t.Fatalf("read %d NUMA entries, want 2: %+v", len(memory.NUMANodes), memory.NUMANodes)
	}
	for i, node := range memory.NUMANodes {
		if node.Node != i {
			t.Errorf("entry %d is node %d, want them ascending", i, node.Node)
		}
		if node.TotalBytes != 131921280*1024 {
			t.Errorf("node %d has %d bytes, want %d", node.Node, node.TotalBytes,
				uint64(131921280)*1024)
		}
		if node.FreeBytes != 2097152*1024 {
			t.Errorf("node %d has %d free, want %d", node.Node, node.FreeBytes,
				uint64(2097152)*1024)
		}
	}
}

func TestReadMemoryOrdersNodesByIDAndNotByName(t *testing.T) {
	// The same trap the huge-page reader had: node10 sorts before node2 by
	// name, and a caller reading NUMANodes[j] as node j would read another
	// node's free memory.
	f := fixture{files: map[string]string{"meminfo": "MemTotal: 1024 kB"}}
	for node := range 11 {
		f.files[nodeMeminfoPath(node)] = nodeMeminfo(node)
	}

	root := f.write(t)
	memory, err := ReadMemory(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the memory: %v", err)
	}

	if len(memory.NUMANodes) != 11 {
		t.Fatalf("read %d NUMA entries, want 11", len(memory.NUMANodes))
	}
	for i, node := range memory.NUMANodes {
		if node.Node != i {
			t.Fatalf("entry %d is node %d, want node %d", i, node.Node, i)
		}
		if node.FreeBytes != uint64(i)*1024 {
			t.Errorf("node %d reports %d free, which belongs to another node",
				node.Node, node.FreeBytes)
		}
	}
}

func TestReadMemoryReportsWhatItHasOnAHostWithoutNUMA(t *testing.T) {
	// A kernel without CONFIG_NUMA exports no node directories. The totals are
	// still the machine's, and reporting none of it because the split is
	// missing would lose the reading that matters most.
	root := fixture{files: map[string]string{
		"meminfo": "MemTotal:  1048576 kB\nMemFree:  524288 kB\nMemAvailable:  786432 kB",
	}}.write(t)

	memory, err := ReadMemory(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the memory of a host with no NUMA: %v", err)
	}
	if memory.TotalBytes != 1048576*1024 {
		t.Errorf("read %d bytes total", memory.TotalBytes)
	}
	if len(memory.NUMANodes) != 0 {
		t.Errorf("read %d NUMA entries on a host that exports none", len(memory.NUMANodes))
	}
}

func TestReadMemoryRefusesATreeWithNoMeminfo(t *testing.T) {
	// A host whose memory could not be read is not a host with no memory, and
	// reporting zero would let a caller size a storage node against it.
	root := fixture{files: map[string]string{"unrelated": "x"}}.write(t)

	if _, err := ReadMemory(Config{SysfsRoot: root, ProcRoot: root}); err == nil {
		t.Error("read the memory of a tree that has no meminfo; zero bytes is a " +
			"machine nothing can run on rather than a reading")
	}
}

// nodeMeminfoPath and nodeMeminfo build one node's file, with the free memory
// set to the node's own id so a misordered reading is visible.
func nodeMeminfoPath(node int) string {
	return "devices/system/node/node" + itoa(node) + "/meminfo"
}

func nodeMeminfo(node int) string {
	return "Node " + itoa(node) + " MemTotal:       1024 kB\n" +
		"Node " + itoa(node) + " MemFree:        " + itoa(node) + " kB"
}

// itoa keeps the fixture free of a strconv import.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
