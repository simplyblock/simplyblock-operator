// The captured-fleet cases: two real fleets, read by the real readers.
//
// Every other family in this package states a machine in Go — these disks, this
// NUMA layout, this NIC — which is what a case about one mutation wants. This
// family states none of it. It replays the sysfs of machines a run of the e2e
// suite actually ran on, hands it to inventory.Collect, and turns what comes
// back into the report a probe would have written on those machines.
//
// What that covers is the half no synthetic fixture can: the shape of a real
// host. A GCP worker running K3s presents sixteen nbd devices nobody asked for,
// a boot disk carrying a partition table, a flannel bridge and a veth pair
// beside the one physical NIC, and — on the NVMe fleet — a single controller
// exporting two namespaces, which is the case a hand-written fixture gets right
// only by knowing to write it. A reader that starts disagreeing with a machine
// says so here rather than on a cluster.
//
// The two fleets are the two device classes, captured from the runs that
// exercise them: gs://simplyblock-e2e-test-logs-gcp/35988660084 for the NVMe
// fleet and .../35975993533 for the block one.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/inventory/transcript"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// capturedHosts is where the transcripts live, relative to this package.
const capturedHosts = "hack/discoveryfixtures/testdata/hosts"

// statedMemory is what a captured fleet's workers are said to have.
//
// It is the one reading a transcript cannot supply: capture-host.py walks the
// trees the readers walk under /sys, and memory is read from /proc/meminfo,
// which is not one of them. The figures below are the machine type's, checked
// against what the capture does carry — sixteen CPUs and 31 GiB of 2 MiB huge
// pages already reserved on the NVMe fleet, eight and 15 GiB on the block one —
// so the memory a case states and the huge pages it read agree with each other.
//
// Everything else in these reports is the machine's own.
type statedMemory struct {
	// TotalBytes is the machine's RAM, and AvailableBytes what is left with the
	// huge pages the capture recorded already taken out of it.
	TotalBytes     uint64
	AvailableBytes uint64
}

// capturedFleet is every transcript in one directory, read by the real readers
// and turned into the reports a probe would have written.
//
// The management address is handed out by position rather than derived from the
// name, because these are real names and two of them end in the same digit: the
// extra node and the first storage node are both -1, and an address keyed off
// that would have the fleet's workers claiming one address.
func capturedFleet(dir string, memory statedMemory) []nodeprobe.Report {
	paths, err := filepath.Glob(filepath.Join(capturedHosts, dir, "*.json.gz"))
	if err != nil || len(paths) == 0 {
		panic(fmt.Sprintf("no transcripts under %s/%s: %v", capturedHosts, dir, err))
	}
	sort.Strings(paths)

	reports := make([]nodeprobe.Report, 0, len(paths))
	for i, path := range paths {
		name := filepath.Base(path)
		name = name[:len(name)-len(".json.gz")]
		reports = append(reports, capturedHost(path, name, fleetAddress(i), memory))
	}
	return reports
}

// fleetAddress is the management address of the worker at this position.
func fleetAddress(index int) string { return fmt.Sprintf("10.10.10.%d", index+1) }

// capturedHost replays one transcript and reads it.
//
// The collect error is passed through as the report's unreadable field rather
// than swallowed or panicked on, because that is what the probe does with it: a
// reading that failed on a real host reaches the operator as a report saying
// so, and a case built from a capture should reach it the same way.
func capturedHost(path, name, address string, memory statedMemory) nodeprobe.Report {
	host, err := transcript.Load(path)
	if err != nil {
		panic(err.Error())
	}

	root, err := os.MkdirTemp("", "discoveryfixtures-host-")
	if err != nil {
		panic(err.Error())
	}
	defer func() { _ = os.RemoveAll(root) }()

	// class/block, which the scan reads, is derived rather than captured: the
	// walker that took these transcripts seeds block/ instead. See
	// transcript.WithClassBlock.
	if err := host.WithClassBlock().Materialize(root); err != nil {
		panic(err.Error())
	}
	if err := writeStatedProc(root, memory); err != nil {
		panic(err.Error())
	}

	inventoryOf, unreadable := inventory.Collect(context.Background(), inventory.Config{
		SysfsRoot: root,
		ProcRoot:  root,
		DevRoot:   transcript.DevRootOf(root),
		// What each disk carries is stated blank rather than read, and the two
		// reasons are the same reason. A transcript captures sysfs and not the
		// disks themselves, so there is nothing to read; and the local reader is
		// Linux-only, so a fixture that read would say "this is darwin" or not,
		// depending on who generated it. Blank is what these disks held: the
		// fleet was captured on fresh machines before a cluster took them.
		Prober: blockdev.NewProberWithOpener(blankDevice),
		// And whether the kernel would hand the device over is stated the same
		// way, for the same two reasons: there is no kernel holding these disks
		// here, and the real question is Linux-only. Free is what they were.
		Exclusive: func(string) error { return nil },
		// The addresses a host holds are the kernel's to answer over netlink,
		// which a transcript does not capture and a fixture must therefore
		// state. It is the management address of the node object this worker
		// gets, so the two agree the way they do on a machine.
		InterfaceAddresses: func() (map[string][]string, error) {
			return map[string][]string{"eth0": {address}}, nil
		},
	})
	// The path a device is at on the machine, rather than inside the temporary
	// tree this transcript was replayed in: the scan builds it from DevRoot, and
	// a fixture carrying one reviewer's scratch directory would be rewritten by
	// the next reviewer to carry theirs.
	for i := range inventoryOf.Devices {
		inventoryOf.Devices[i].Path = filepath.Join(blockdev.DefaultDevRoot, inventoryOf.Devices[i].Name)
	}
	return nodeprobe.FromInventory(name, probedAt.Time, inventoryOf, unreadable)
}

// blankDevice opens a device that reads as zeros, which is a blank disk. See
// the Prober above.
func blankDevice(context.Context, blockdev.Device) (blockdev.Reader, error) {
	return zeroReader{}, nil
}

// zeroReader answers every read with zeros and never fails.
type zeroReader struct{}

func (zeroReader) ReadAt(_ context.Context, p []byte, _ int64) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func (zeroReader) Close() error { return nil }

// writeStatedProc writes the /proc entries the readers want and the transcript
// does not carry: the memory above, and an empty mount table.
//
// The mount table is empty rather than invented. On the machine, the boot disk
// carries the root filesystem and is refused for being mounted as well as for
// carrying a partition table; here only the partition table is left, so the
// disk is refused for one of the two reasons it would really be refused for.
// Writing a mount table naming devices nobody checked would be a fixture
// asserting something about a host that nothing in the capture supports.
func writeStatedProc(root string, memory statedMemory) error {
	meminfo := fmt.Sprintf(
		"MemTotal:       %d kB\nMemFree:        %d kB\nMemAvailable:   %d kB\n",
		memory.TotalBytes/1024, memory.AvailableBytes/1024, memory.AvailableBytes/1024)
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte(meminfo), 0o644); err != nil {
		return fmt.Errorf("write the stated meminfo: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		return fmt.Errorf("create the stated proc self: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "self", "mountinfo"), nil, 0o644); err != nil {
		return fmt.Errorf("write the stated mount table: %w", err)
	}
	return nil
}

// capturedNodes is what Kubernetes says about a captured fleet: the names the
// transcripts were taken under, at the addresses their reports hold.
func capturedNodes(reports []nodeprobe.Report) []corev1.Node {
	nodes := make([]corev1.Node, 0, len(reports))
	for i, report := range reports {
		nodes = append(nodes, kubeNode(report.Node, reachableAt(fleetAddress(i))))
	}
	return nodes
}

func hostCases() map[string]Case {
	// n2-standard-16, with the 31 GiB of huge pages the capture recorded
	// already reserved out of its 64.
	nvmeMemory := statedMemory{TotalBytes: 64 * gb, AvailableBytes: 30 * gb}
	// n2-standard-8, the same way: 32 GiB with 15 reserved.
	blockMemory := statedMemory{TotalBytes: 32 * gb, AvailableBytes: 15 * gb}
	// The QEMU worker: 64 GiB, all of it available, because this capture
	// recorded no huge pages reserved at all. It is the one figure of the three
	// that is a round number rather than a machine type's, since the host is
	// somebody's hypervisor and not a catalog entry.
	qemuMemory := statedMemory{TotalBytes: 64 * gb, AvailableBytes: 64 * gb}

	nvmeFleet := capturedFleet("gcp-k3s-nvme", nvmeMemory)
	blockFleet := capturedFleet("gcp-k3s-block", blockMemory)
	qemuFleet := capturedFleet("qemu-nvme-with-optical", qemuMemory)

	return map[string]Case{
		"HOST-01": {
			Family:  "host",
			Slug:    "a-captured-nvme-fleet-on-gcp",
			Reports: nvmeFleet,
			Nodes:   capturedNodes(nvmeFleet),
			Note: "The six storage workers of e2e run 35988660084 and the extra worker " +
				"beside them, replayed from sysfs. Each storage worker has one NVMe " +
				"controller exporting two namespaces, a partitioned boot disk on the " +
				"virtio-scsi bus, and sixteen nbd devices; the extra worker has only its " +
				"own boot disk, which is an NVMe namespace. Memory is stated rather than " +
				"captured; see statedMemory. Both captured cases record vcpuCount at the " +
				"API's minimum of 4 on machines of 16 and 8 cores, because these hosts " +
				"place no device on a memory node -- GCP exports no NUMA affinity for " +
				"either bus -- and the sizing reads the cores of the node a device sits " +
				"on. No synthetic case shows it, because every one of them states a " +
				"memory node per device.",
		},
		"HOST-02": {
			Family:   "host",
			Slug:     "a-captured-block-fleet-on-gcp",
			Reports:  blockFleet,
			Nodes:    capturedNodes(blockFleet),
			Discover: blockClass(),
			Note: "The three storage workers of e2e run 35975993533 and the extra worker " +
				"beside them. Each storage worker has two unpartitioned virtio disks " +
				"beside a partitioned boot disk and no NVMe at all, which is the fleet " +
				"the block class exists for.",
		},
		"HOST-03": {
			Family:   "host",
			Slug:     "a-captured-qemu-worker-with-an-optical-drive",
			Reports:  qemuFleet,
			Nodes:    capturedNodes(qemuFleet),
			Discover: blockClass(),
			Note: "One worker off a QEMU host rather than a cloud one, and the family's " +
				"smallest fleet. It carries two devices no cloud image presents, and each " +
				"is why it is here. Its two Samsung 1.92 TB NVMe disks are the case for " +
				"the block class taking local NVMe: the classes are not disjoint sets of " +
				"hardware, and a machine whose only real storage is NVMe has to be " +
				"deployable as logical block devices. Its QEMU DVD-ROM is the case for " +
				"refusing a removable device: with install media in it the drive reports " +
				"a whole disk of 924 MB on the SATA bus and reads as blank, so every " +
				"other ground admits it. Beside them are a partitioned virtio boot disk " +
				"and sixteen nbd devices. Memory is stated rather than captured; see " +
				"statedMemory.",
		},
	}
}
