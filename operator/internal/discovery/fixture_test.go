// The probe reports the pipeline tests are run over.
//
// They are built rather than captured because what varies between the cases is
// exactly one thing each time — which memory node a disk hangs off, whether a
// worker's slots match its neighbor's, what the probe already refused — and a
// captured tree would fix all of them at once. The shapes themselves are real:
// four NVMe disks with GPT tables on a two-socket machine is the fleet this was
// developed against.

package discovery

import (
	"fmt"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// disk is one free NVMe disk in a slot, on a memory node.
func disk(name, pci string, numaNode int, sizeBytes uint64) nodeprobe.Device {
	return nodeprobe.Device{
		Name:       name,
		Path:       "/dev/" + name,
		PCIAddress: pci,
		SizeBytes:  sizeBytes,
		Kind:       string(blockdev.KindDisk),
		Transport:  string(blockdev.TransportNVMe),
		Model:      "SAMSUNG MZQL23T8HCLS-00A07",
		NUMANode:   numaNode,
		Available:  true,
		Content:    "Blank",
	}
}

// refused is a device the probe declined, for the grounds given.
func refused(d nodeprobe.Device, reasons ...blockdev.Reason) nodeprobe.Device {
	d.Available = false
	d.Content = ""
	for _, reason := range reasons {
		d.Rejections = append(d.Rejections, nodeprobe.Rejection{
			Reason: string(reason), Detail: "as the probe found it",
		})
	}
	return d
}

// blockDisk is a free disk of the other class: a virtio disk with a path and no
// PCI address the draft would name it by.
func blockDisk(name string, numaNode int, sizeBytes uint64) nodeprobe.Device {
	return nodeprobe.Device{
		Name:      name,
		Path:      "/dev/" + name,
		SizeBytes: sizeBytes,
		Kind:      string(blockdev.KindDisk),
		Transport: string(blockdev.TransportVirtio),
		NUMANode:  numaNode,
		Available: true,
		Content:   "Blank",
	}
}

// report is a worker with the devices given, two memory nodes, and a data NIC
// on each.
func report(node string, devices ...nodeprobe.Device) nodeprobe.Report {
	return nodeprobe.Report{
		Version: nodeprobe.ReportVersion,
		Node:    node,
		CPU: nodeprobe.CPU{
			OnlineCPUs:     32,
			PhysicalCores:  16,
			Sockets:        2,
			ThreadsPerCore: 2,
			HyperThreading: true,
			NUMANodes: []nodeprobe.NUMACPUs{
				{Node: 0, OnlineCPUs: rangeOf(0, 16), PhysicalCores: 8},
				{Node: 1, OnlineCPUs: rangeOf(16, 32), PhysicalCores: 8},
			},
		},
		HugePages: []nodeprobe.HugePagePool{{
			SizeBytes: 1 << 30, Total: 32, Free: 32,
			NUMANodes: []nodeprobe.NUMAHugePages{
				{Node: 0, Total: 16, Free: 16},
				{Node: 1, Total: 16, Free: 16},
			},
		}},
		Interfaces: []nodeprobe.Interface{
			{Name: "eth0", SpeedMbps: 25000, State: "up", NUMANode: 0, Driver: "mlx5_core"},
			{Name: "eth1", SpeedMbps: 25000, State: "up", NUMANode: 1, Driver: "mlx5_core"},
		},
		Devices: devices,
	}
}

// rangeOf is the CPU ids from low up to but not including high.
func rangeOf(low, high int) []int {
	ids := make([]int, 0, high-low)
	for id := low; id < high; id++ {
		ids = append(ids, id)
	}
	return ids
}

// uniformFleet is n workers with the same four disks in the same four slots,
// two per memory node: the fleet a single group is the right answer for.
func uniformFleet(n int) []nodeprobe.Report {
	const tb = uint64(1) << 40
	reports := make([]nodeprobe.Report, 0, n)
	for i := 1; i <= n; i++ {
		node := fmt.Sprintf("worker-%d", i)
		reports = append(reports, report(node,
			disk("nvme0n1", "0000:5e:00.0", 0, 3*tb),
			disk("nvme1n1", "0000:5f:00.0", 0, 3*tb),
			disk("nvme2n1", "0000:af:00.0", 1, 3*tb),
			disk("nvme3n1", "0000:b0:00.0", 1, 3*tb),
		))
	}
	return reports
}

// addresses is the PCI addresses of a group, for an assertion.
func addresses(devices []nodeprobe.Device) []string {
	out := make([]string, 0, len(devices))
	for _, device := range devices {
		out = append(out, device.PCIAddress)
	}
	return out
}
