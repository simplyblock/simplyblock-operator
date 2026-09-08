// What the CPU reader concludes from a topology tree, and what it refuses to
// conclude.
//
// The counts are the interesting part rather than the parsing. A host reports
// its logical CPUs one way and its cores another, and sizing a storage node
// against the wrong one of those two either halves its throughput or oversells
// the machine, so every case below states all four numbers and not just the one
// it is about.

package inventory

import (
	"fmt"
	"slices"
	"testing"
)

// smtHost is a two-socket, four-core, two-thread machine: 16 logical CPUs over
// 8 physical cores. The sibling pairs are interleaved, which is one of the two
// orderings firmware uses and the one a reader that assumes the other gets
// wrong.
func smtHost() fixture {
	f := fixture{files: map[string]string{
		"devices/system/cpu/online":     "0-15",
		"devices/system/cpu/present":    "0-15",
		"devices/system/cpu/smt/active": "1",
		"self/status":                   "Name:\tatlas\nCpus_allowed_list:\t0-15\nMems_allowed_list:\t0-1",
	}}
	for cpu := range 16 {
		core := (cpu / 2) % 4
		socket := cpu / 8
		sibling := cpu ^ 1
		base := fmt.Sprintf("devices/system/cpu/cpu%d/topology/", cpu)
		f.files[base+"core_id"] = fmt.Sprint(core)
		f.files[base+"physical_package_id"] = fmt.Sprint(socket)
		f.files[base+"thread_siblings_list"] = fmt.Sprintf("%d,%d", min(cpu, sibling), max(cpu, sibling))
	}
	f.files["devices/system/node/node0/cpulist"] = "0-7"
	f.files["devices/system/node/node1/cpulist"] = "8-15"
	return f
}

func TestReadCPUCountsCoresAndThreadsSeparately(t *testing.T) {
	root := smtHost().write(t)

	cpu, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the CPU topology: %v", err)
	}

	want := counts{
		online: 16, present: 16, affinity: 16,
		sockets: 2, cores: 8, threadsPerCore: 2,
		hyperThreading: true, numaNodes: 2,
	}
	if got := countsOf(cpu); got != want {
		t.Errorf("read %+v, want %+v", got, want)
	}
}

// counts is the scalar half of a CPU reading, as one comparable value: the
// numbers are only meaningful beside each other, and a test that asserted one
// of them alone would pass while the reading it came from was wrong.
type counts struct {
	online, present, affinity      int
	sockets, cores, threadsPerCore int
	hyperThreading                 bool
	numaNodes                      int
}

func countsOf(cpu CPU) counts {
	return counts{
		online: cpu.OnlineCount, present: cpu.PresentCount, affinity: cpu.AffinityCount,
		sockets: cpu.Sockets, cores: cpu.PhysicalCores, threadsPerCore: cpu.ThreadsPerCore,
		hyperThreading: cpu.HyperThreading, numaNodes: len(cpu.NUMANodes),
	}
}

func TestReadCPUReportsHyperThreadingOffWhenEveryCoreHasOneThread(t *testing.T) {
	f := fixture{files: map[string]string{
		"devices/system/cpu/online":     "0-7",
		"devices/system/cpu/present":    "0-7",
		"devices/system/cpu/smt/active": "0",
		"self/status":                   "Cpus_allowed_list:\t0-7",
	}}
	for cpu := range 8 {
		base := fmt.Sprintf("devices/system/cpu/cpu%d/topology/", cpu)
		f.files[base+"core_id"] = fmt.Sprint(cpu)
		f.files[base+"physical_package_id"] = "0"
		f.files[base+"thread_siblings_list"] = fmt.Sprint(cpu)
	}
	f.files["devices/system/node/node0/cpulist"] = "0-7"

	root := f.write(t)
	cpu, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the CPU topology: %v", err)
	}
	if cpu.HyperThreading {
		t.Error("reported hyperthreading on a host whose cores each have one thread")
	}
	if cpu.ThreadsPerCore != 1 {
		t.Errorf("read %d threads per core, want 1", cpu.ThreadsPerCore)
	}
	if cpu.PhysicalCores != 8 || cpu.Sockets != 1 {
		t.Errorf("read %d cores over %d sockets, want 8 over 1", cpu.PhysicalCores, cpu.Sockets)
	}
}

func TestReadCPUDerivesHyperThreadingWithoutTheSMTAttribute(t *testing.T) {
	f := smtHost()
	delete(f.files, "devices/system/cpu/smt/active")

	root := f.write(t)
	cpu, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the CPU topology: %v", err)
	}
	if !cpu.HyperThreading {
		t.Error("a host whose cores carry two threads each is hyperthreaded, " +
			"whether or not the kernel exports smt/active")
	}
}

func TestReadCPUExcludesOfflineCPUsFromTheTopology(t *testing.T) {
	f := smtHost()
	// The last sibling pair is offline, so its directories carry no topology.
	f.files["devices/system/cpu/online"] = "0-13"
	f.files["devices/system/cpu/present"] = "0-15"
	for _, cpu := range []int{14, 15} {
		base := fmt.Sprintf("devices/system/cpu/cpu%d/topology/", cpu)
		delete(f.files, base+"core_id")
		delete(f.files, base+"physical_package_id")
		delete(f.files, base+"thread_siblings_list")
	}

	root := f.write(t)
	cpu, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the CPU topology: %v", err)
	}
	if cpu.OnlineCount != 14 || cpu.PresentCount != 16 || cpu.OfflineCount != 2 {
		t.Errorf("read %d online of %d present with %d offline, want 14 of 16 with 2 offline",
			cpu.OnlineCount, cpu.PresentCount, cpu.OfflineCount)
	}
	if cpu.PhysicalCores != 7 {
		t.Errorf("read %d physical cores, want 7: an offline thread pair leaves seven usable cores",
			cpu.PhysicalCores)
	}
}

func TestReadCPUReportsTheAffinityItWasGivenSeparately(t *testing.T) {
	f := smtHost()
	f.files["self/status"] = "Cpus_allowed_list:\t0-3"

	root := f.write(t)
	cpu, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the CPU topology: %v", err)
	}
	if cpu.OnlineCount != 16 {
		t.Errorf("read %d online CPUs, want 16: a cpuset narrows this process, not the host",
			cpu.OnlineCount)
	}
	if cpu.AffinityCount != 4 {
		t.Errorf("read an affinity of %d, want 4", cpu.AffinityCount)
	}
}

func TestReadCPUSaysWhichCPUsEachNUMANodeOwns(t *testing.T) {
	// A count of nodes is not the answer a caller needs. A storage node pinned
	// to a socket draws its SPDK cores from that socket's CPUs and its memory
	// from that socket's huge pages, so which CPUs belong to which node is the
	// fact, and the count is what falls out of it.
	root := smtHost().write(t)

	cpu, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the CPU topology: %v", err)
	}

	if len(cpu.NUMANodes) != 2 {
		t.Fatalf("read %d NUMA nodes, want 2: %+v", len(cpu.NUMANodes), cpu.NUMANodes)
	}
	for i, want := range []NUMACPUs{
		{Node: 0, OnlineCPUs: []int{0, 1, 2, 3, 4, 5, 6, 7}, PhysicalCores: 4},
		{Node: 1, OnlineCPUs: []int{8, 9, 10, 11, 12, 13, 14, 15}, PhysicalCores: 4},
	} {
		got := cpu.NUMANodes[i]
		if got.Node != want.Node || got.PhysicalCores != want.PhysicalCores ||
			!slices.Equal(got.OnlineCPUs, want.OnlineCPUs) {
			t.Errorf("NUMA entry %d is %+v, want %+v", i, got, want)
		}
	}
}

func TestReadCPUCountsOnlyTheOnlineCPUsOfANUMANode(t *testing.T) {
	// A node's cpulist names every CPU the firmware attached to it, online or
	// not, so an offline pair has to come out of the node it belonged to as
	// well as out of the host's total.
	f := smtHost()
	f.files["devices/system/cpu/online"] = "0-13"
	for _, cpu := range []int{14, 15} {
		base := fmt.Sprintf("devices/system/cpu/cpu%d/topology/", cpu)
		delete(f.files, base+"core_id")
		delete(f.files, base+"physical_package_id")
		delete(f.files, base+"thread_siblings_list")
	}

	root := f.write(t)
	cpu, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the CPU topology: %v", err)
	}

	if len(cpu.NUMANodes) != 2 {
		t.Fatalf("read %d NUMA nodes, want 2", len(cpu.NUMANodes))
	}
	second := cpu.NUMANodes[1]
	if !slices.Equal(second.OnlineCPUs, []int{8, 9, 10, 11, 12, 13}) {
		t.Errorf("node 1 owns %v, want [8 9 10 11 12 13]: the offline pair is "+
			"in its cpulist and is not usable", second.OnlineCPUs)
	}
	if second.PhysicalCores != 3 {
		t.Errorf("node 1 has %d cores, want 3", second.PhysicalCores)
	}
	if first := cpu.NUMANodes[0]; len(first.OnlineCPUs) != 8 || first.PhysicalCores != 4 {
		t.Errorf("node 0 is %+v, and nothing about it changed", first)
	}
}

func TestReadCPUReportsOneNodeOnAHostWithoutNUMA(t *testing.T) {
	// A kernel built without CONFIG_NUMA exports no node directories, and every
	// CPU on such a host is on the one node userspace calls node 0. Reporting
	// no nodes at all would leave a caller unable to place anything.
	f := smtHost()
	delete(f.files, "devices/system/node/node0/cpulist")
	delete(f.files, "devices/system/node/node1/cpulist")

	root := f.write(t)
	cpu, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root})
	if err != nil {
		t.Fatalf("read the CPU topology: %v", err)
	}

	if len(cpu.NUMANodes) != 1 {
		t.Fatalf("read %d NUMA nodes on a host that exports none, want 1: %+v",
			len(cpu.NUMANodes), cpu.NUMANodes)
	}
	only := cpu.NUMANodes[0]
	if only.Node != 0 || len(only.OnlineCPUs) != 16 || only.PhysicalCores != 8 {
		t.Errorf("the one node is %+v, want node 0 with all 16 CPUs over 8 cores", only)
	}
}

func TestReadCPURefusesATreeWithNoCPUs(t *testing.T) {
	root := fixture{files: map[string]string{"self/status": "Cpus_allowed_list:\t0"}}.write(t)

	if _, err := ReadCPU(Config{SysfsRoot: root, ProcRoot: root}); err == nil {
		t.Error("read a CPU topology from a tree that has none; a host with " +
			"zero cores is an unreadable tree rather than a fact worth reporting")
	}
}

func TestParseCPUList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
	}{
		{"", nil},
		{"0", []int{0}},
		{"0-3", []int{0, 1, 2, 3}},
		{"0-1,8,10-11", []int{0, 1, 8, 10, 11}},
		{" 2 , 4 ", []int{2, 4}},
	} {
		got, err := parseCPUList(tc.in)
		if err != nil {
			t.Errorf("parse %q: %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parse %q: read %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parse %q: read %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}

	for _, bad := range []string{"3-1", "a", "1-", "-1"} {
		if _, err := parseCPUList(bad); err == nil {
			t.Errorf("parsed %q as a CPU list", bad)
		}
	}
}
