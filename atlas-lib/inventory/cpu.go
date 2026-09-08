// How many processors a host has, in the four senses that differ.
//
// Logical CPUs, physical cores, sockets, and the CPUs this process is allowed
// to run on are four different numbers on the same machine, and a storage node
// sized against the wrong one either leaves half the hardware idle or oversells
// it. This file reads all four and names each in the field that carries it,
// rather than picking one and calling it the CPU count.
//
// Everything is derived from the online CPUs. An offline CPU exports no
// topology, so counting it as a core would invent one, and the kernel's own
// online mask is the list this walks.

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

// CPU is a host's processor inventory.
type CPU struct {
	// OnlineCount is the number of logical CPUs the scheduler can currently
	// place work on. It is the number an SPDK core mask is drawn from.
	OnlineCount int

	// PresentCount is the number of logical CPUs the kernel knows about,
	// online or not.
	PresentCount int

	// OfflineCount is PresentCount less OnlineCount, stated rather than left to
	// be subtracted, because a host with CPUs offline is worth noticing.
	OfflineCount int

	// AffinityCount is how many of the online CPUs this process may actually
	// run on. It is smaller than OnlineCount when a cpuset confines the
	// process, which is the normal case for a pod, and it is not the host's
	// capacity: the two are separate fields because reading one as the other is
	// the mistake this type exists to prevent.
	AffinityCount int

	// Sockets is the number of distinct physical packages.
	Sockets int

	// PhysicalCores is the number of distinct cores across all sockets,
	// counting a hyperthreaded core once.
	PhysicalCores int

	// ThreadsPerCore is OnlineCount divided by PhysicalCores, which is 1 on a
	// host without simultaneous multithreading and 2 on almost every host with
	// it.
	ThreadsPerCore int

	// HyperThreading reports whether simultaneous multithreading is active. It
	// is the kernel's own answer where the kernel gives one, and is derived
	// from the thread siblings where it does not.
	HyperThreading bool

	// NUMANodes is one entry per memory node, ascending by node, naming which
	// of the online CPUs each one owns.
	//
	// The count of nodes is not enough on its own. A storage node is pinned to
	// a socket and draws its SPDK cores from that socket's CPUs and its memory
	// from that socket's huge pages, so which CPUs belong to which node is the
	// fact a placement decision needs, and the count is what falls out of it.
	//
	// It is never empty for a reading that succeeded: a host that exports no
	// node directories has one node holding every CPU.
	NUMANodes []NUMACPUs
}

// NUMACPUs is one memory node's share of the host's processors.
//
// It parallels NUMAHugePages, which is one node's share of a huge-page pool,
// and Interface.NUMANode and blockdev.Disk.NUMANode, which say which node a
// NIC and a disk hang off. Together, they are what lets a caller place a
// storage node where its cores, its memory, its NIC, and its disks are all on
// the same side of the interconnect.
type NUMACPUs struct {
	// Node is the memory node's id, as in devices/system/node/nodeN.
	Node int

	// OnlineCPUs is the logical CPUs this node owns that are online, ascending.
	// A node's cpulist names every CPU the firmware attached to it, offline
	// ones included, and an offline CPU is not one work can be placed on.
	OnlineCPUs []int

	// PhysicalCores is how many distinct cores those CPUs sit on, counting a
	// hyperthreaded core once.
	PhysicalCores int
}

// ReadCPU reads a host's processor inventory.
//
// A tree with no online CPUs is an unreadable tree rather than a host with no
// processors, so it is an error: reporting zero cores as a finding would let a
// discovery run write a worker nothing can be placed on.
func ReadCPU(cfg Config) (CPU, error) {
	base := cfg.sysfsPath("devices/system/cpu")

	online, err := readCPUListAttr(filepath.Join(base, "online"))
	if err != nil {
		return CPU{}, err
	}
	if len(online) == 0 {
		return CPU{}, fmt.Errorf("%s lists no online CPUs", filepath.Join(base, "online"))
	}

	present, err := readCPUListAttr(filepath.Join(base, "present"))
	if err != nil {
		// Every kernel that exports online exports present, but the counts a
		// caller sizes against come from online, so a missing present is worth
		// falling back on rather than failing over.
		present = online
	}

	cpu := CPU{
		OnlineCount:  len(online),
		PresentCount: len(present),
	}
	if cpu.OfflineCount = cpu.PresentCount - cpu.OnlineCount; cpu.OfflineCount < 0 {
		cpu.OfflineCount = 0
	}

	topology, siblings, err := readTopology(base, online)
	if err != nil {
		return CPU{}, err
	}
	cpu.Sockets = len(socketsOf(topology, online))
	cpu.PhysicalCores = len(coresOf(topology, online))
	if cpu.PhysicalCores > 0 {
		cpu.ThreadsPerCore = cpu.OnlineCount / cpu.PhysicalCores
	}
	cpu.HyperThreading = hyperThreading(base, siblings, cpu.ThreadsPerCore)

	cpu.NUMANodes = readNUMACPUs(cfg, topology, online)

	allowed, err := readAffinity(cfg)
	if err != nil {
		return CPU{}, err
	}
	cpu.AffinityCount = allowed

	return cpu, nil
}

// core identifies one physical core by the pair of its package and its core id,
// because core_id is unique within a socket and not across them: counting core
// ids alone would collapse a two-socket host's cores into one socket's worth.
type core struct{ socket, id int }

// unknownSocket is the package of a CPU whose topology the kernel does not
// export. It is negative so that it cannot collide with a real package id.
const unknownSocket = -1

// readTopology reads which core each online CPU sits on, and reports the widest
// thread-sibling list it saw.
//
// The map is what every count in this file derives from, host-wide and per NUMA
// node alike. Walking once rather than once per node is not only cheaper: it is
// what keeps a node's core count and the host's from being computed two
// different ways.
func readTopology(base string, online []int) (map[int]core, int, error) {
	topology := make(map[int]core, len(online))
	widestSiblings := 0

	for _, n := range online {
		dir := filepath.Join(base, fmt.Sprintf("cpu%d", n), "topology")
		pkg, err := sysfs.ReadAttr(dir, "physical_package_id")
		if err != nil {
			// A CPU listed online whose topology is absent is a kernel that
			// does not export it (some virtual machines, some ARM64), not a
			// broken host. It still counts as a core of its own, on the
			// package nothing named.
			topology[n] = core{socket: unknownSocket, id: n}
			continue
		}
		socket, err := strconv.Atoi(pkg)
		if err != nil {
			return nil, 0, fmt.Errorf("read %s/physical_package_id: %w", dir, err)
		}
		topology[n] = core{socket: socket, id: sysfs.Int(n, dir, "core_id")}

		if list, err := sysfs.ReadAttr(dir, "thread_siblings_list"); err == nil {
			if ids, err := parseCPUList(list); err == nil && len(ids) > widestSiblings {
				widestSiblings = len(ids)
			}
		}
	}
	return topology, widestSiblings, nil
}

// socketsOf and coresOf are the distinct packages and the distinct cores among
// a set of CPUs. Both take the CPUs to count over, which is how the host's
// totals and one NUMA node's share come out of the same two functions.
func socketsOf(topology map[int]core, cpus []int) map[int]struct{} {
	sockets := map[int]struct{}{}
	for _, n := range cpus {
		if c, ok := topology[n]; ok && c.socket != unknownSocket {
			sockets[c.socket] = struct{}{}
		}
	}
	if len(sockets) == 0 && len(cpus) > 0 {
		// Nothing said which package these CPUs are on, and a host with CPUs
		// has at least one.
		sockets[0] = struct{}{}
	}
	return sockets
}

func coresOf(topology map[int]core, cpus []int) map[core]struct{} {
	cores := map[core]struct{}{}
	for _, n := range cpus {
		if c, ok := topology[n]; ok {
			cores[c] = struct{}{}
		}
	}
	return cores
}

// hyperThreading decides whether SMT is on.
//
// The kernel's smt/active is the direct answer and is preferred, because it
// says whether the sibling threads are usable and not merely present: a host
// booted with nosmt still reports two siblings per core in some topologies. It
// is absent on architectures that never had SMT and on older kernels, and the
// sibling width is then the answer.
func hyperThreading(base string, widestSiblings, threadsPerCore int) bool {
	if active, err := sysfs.ReadAttr(base, "smt", "active"); err == nil {
		return strings.TrimSpace(active) == "1"
	}
	return widestSiblings > 1 || threadsPerCore > 1
}

// readNUMACPUs reads which of the online CPUs each memory node owns.
//
// A node's cpulist names every CPU the firmware attached to it, offline ones
// included, so the list is intersected with the online set rather than reported
// as it stands: an offline CPU is not one work can be placed on, and a node
// reporting it would be oversold by exactly that much.
//
// A host that exports no node directories, which is a kernel built without
// CONFIG_NUMA, has one node holding every CPU. Reporting no nodes at all would
// leave a caller with nowhere to place anything, and node 0 is what userspace
// calls the only node on such a machine.
func readNUMACPUs(cfg Config, topology map[int]core, online []int) []NUMACPUs {
	base := cfg.sysfsPath(nodeDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return []NUMACPUs{wholeHostAsOneNode(topology, online)}
	}

	onlineSet := make(map[int]struct{}, len(online))
	for _, n := range online {
		onlineSet[n] = struct{}{}
	}

	var nodes []NUMACPUs
	for _, entry := range entries {
		id, isNode := nodeID(entry.Name())
		if !isNode {
			continue
		}
		list, err := sysfs.ReadAttr(base, entry.Name(), "cpulist")
		if err != nil {
			continue
		}
		attached, err := parseCPUList(list)
		if err != nil {
			continue
		}

		node := NUMACPUs{Node: id}
		for _, n := range attached {
			if _, up := onlineSet[n]; up {
				node.OnlineCPUs = append(node.OnlineCPUs, n)
			}
		}
		slices.Sort(node.OnlineCPUs)
		node.PhysicalCores = len(coresOf(topology, node.OnlineCPUs))
		nodes = append(nodes, node)
	}

	if len(nodes) == 0 {
		return []NUMACPUs{wholeHostAsOneNode(topology, online)}
	}
	slices.SortFunc(nodes, func(a, b NUMACPUs) int { return cmp.Compare(a.Node, b.Node) })
	return nodes
}

// wholeHostAsOneNode is the reading for a host whose memory nodes could not be
// read: every online CPU, on node 0.
func wholeHostAsOneNode(topology map[int]core, online []int) NUMACPUs {
	cpus := slices.Clone(online)
	slices.Sort(cpus)
	return NUMACPUs{Node: 0, OnlineCPUs: cpus, PhysicalCores: len(coresOf(topology, cpus))}
}

// nodeID reads the id out of a node directory's name, and reports whether the
// name was one at all: node3 is a memory node and possible is not.
func nodeID(name string) (int, bool) {
	rest, isNode := strings.CutPrefix(name, "node")
	if !isNode {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	if err != nil || id < 0 {
		return 0, false
	}
	return id, true
}

// readAffinity reads the CPUs this process may run on, from the status file
// rather than from sched_getaffinity, so that the reading follows Config's
// roots like every other reading here.
func readAffinity(cfg Config) (int, error) {
	status, err := os.ReadFile(cfg.procPath("self", "status"))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", cfg.procPath("self", "status"), err)
	}
	for line := range strings.Lines(string(status)) {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), "Cpus_allowed_list:")
		if !found {
			continue
		}
		ids, err := parseCPUList(strings.TrimSpace(rest))
		if err != nil {
			return 0, fmt.Errorf("parse Cpus_allowed_list: %w", err)
		}
		return len(ids), nil
	}
	return 0, fmt.Errorf("%s carries no Cpus_allowed_list", cfg.procPath("self", "status"))
}

// readCPUListAttr reads a sysfs attribute holding a CPU list.
func readCPUListAttr(path string) ([]int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	ids, err := parseCPUList(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return ids, nil
}

// parseCPUList expands the kernel's range notation (0-1,8,10-11) into the ids
// it names. An empty string is an empty list, which is what the kernel
// writes for a host with nothing offline.
func parseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	var ids []int
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("%q is not a CPU id", part)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("%q is not a CPU range", part)
			}
		}
		if last < first {
			return nil, fmt.Errorf("%q counts backward", part)
		}
		for id := first; id <= last; id++ {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
