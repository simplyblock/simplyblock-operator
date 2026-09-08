// Turning what atlas/inventory read into what the probe reports.
//
// It is a translation and nothing more: no filtering, no judgment, and no
// device dropped. A discovery run's device filter is applied by the operator
// that holds the whole fleet's reports, not here, because the filter is an
// input to the run and the report is the evidence the run rests on. A probe
// that had already narrowed its answer would leave an administrator reading a
// narrowed report that never said what it left out.

package nodeprobe

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/pci"
)

// FromInventory renders a collected inventory as the report for one node.
//
// The error is the one Collect returned beside the inventory, and it belongs in
// the report rather than replacing it: what a partial collection produced is
// still what the worker has, and the failures are recorded so a reviewer knows
// which part of the answer is missing.
func FromInventory(node string, at time.Time, inv inventory.Inventory, unreadable error) Report {
	return Report{
		Version:         ReportVersion,
		Node:            node,
		ProbedAt:        metav1.NewTime(at),
		CPU:             cpuOf(inv.CPU),
		HugePages:       hugePagesOf(inv.HugePages),
		Interfaces:      interfacesOf(inv.Interfaces),
		Devices:         devicesOf(inv.Devices),
		NVMeControllers: controllersOf(inv.NVMeControllers),
		Unreadable:      sentences(unreadable),
	}
}

func cpuOf(cpu inventory.CPU) CPU {
	out := CPU{
		OnlineCPUs:     cpu.OnlineCount,
		OfflineCPUs:    cpu.OfflineCount,
		PhysicalCores:  cpu.PhysicalCores,
		Sockets:        cpu.Sockets,
		ThreadsPerCore: cpu.ThreadsPerCore,
		HyperThreading: cpu.HyperThreading,
	}
	for _, node := range cpu.NUMANodes {
		out.NUMANodes = append(out.NUMANodes, NUMACPUs{
			Node:          node.Node,
			OnlineCPUs:    node.OnlineCPUs,
			PhysicalCores: node.PhysicalCores,
		})
	}
	return out
}

func hugePagesOf(pages inventory.HugePages) []HugePagePool {
	out := make([]HugePagePool, 0, len(pages.Pools))
	for _, pool := range pages.Pools {
		entry := HugePagePool{SizeBytes: pool.SizeBytes, Total: pool.Total, Free: pool.Free}
		for _, share := range pool.NUMANodes {
			entry.NUMANodes = append(entry.NUMANodes, NUMAHugePages{
				Node: share.Node, Total: share.Total, Free: share.Free,
			})
		}
		out = append(out, entry)
	}
	return out
}

func interfacesOf(ifaces []inventory.Interface) []Interface {
	out := make([]Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		out = append(out, Interface{
			Name:       iface.Name,
			MACAddress: iface.MACAddress,
			SpeedMbps:  iface.SpeedMbps,
			MTU:        iface.MTU,
			State:      string(iface.OperState),
			Driver:     iface.Driver,
			PCIAddress: iface.PCIAddress,
			NUMANode:   iface.NUMANode,
			Virtual:    iface.Virtual,
			Loopback:   iface.Loopback,
		})
	}
	return out
}

func devicesOf(candidates []blockdev.Candidate) []Device {
	out := make([]Device, 0, len(candidates))
	for _, c := range candidates {
		device := Device{
			Name:        c.Name,
			Path:        c.Path,
			PCIAddress:  c.PCIAddress,
			SizeBytes:   c.SizeBytes,
			Kind:        string(c.Kind),
			Transport:   string(c.Transport),
			Vendor:      c.Vendor,
			Model:       c.Model,
			Serial:      c.Serial,
			Rotational:  c.Rotational,
			NUMANode:    c.NUMANode,
			Available:   c.Available(),
			ContentType: c.Reading.Type,
		}
		// A device refused before anything was opened carries ContentUnknown,
		// and the report leaves the field empty rather than writing "Unknown":
		// an absent reading is what happened, and a named one would read as a
		// device that was checked.
		if c.Reading.Content != blockdev.ContentUnknown {
			device.Content = c.Reading.Content.String()
		}
		for _, rejection := range c.Rejections {
			device.Rejections = append(device.Rejections, Rejection{
				Reason: string(rejection.Reason),
				Detail: rejection.Detail,
			})
		}
		out = append(out, device)
	}
	return out
}

func controllersOf(devices []pci.Device) []Controller {
	out := make([]Controller, 0, len(devices))
	for _, device := range devices {
		out = append(out, Controller{
			Address:          device.Address,
			Driver:           device.Driver,
			Vendor:           device.Vendor,
			Product:          device.Product,
			NUMANode:         device.NUMANode,
			TakenByUserspace: device.BoundToUserspace(),
		})
	}
	return out
}

// sentences flattens the error Collect joins into one entry per reader that
// failed.
//
// errors.Join builds a tree, so this walks it: a caller reading the joined
// Error() string would get one blob with embedded newlines, and the report says
// what could not be read as a list because that is what it is.
func sentences(err error) []string {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []string
		for _, inner := range joined.Unwrap() {
			out = append(out, sentences(inner)...)
		}
		return out
	}
	return []string{err.Error()}
}

// Summary is the one line the probe logs when it is done, so that a
// kubectl logs of a finished Job says what it found without anybody parsing
// JSON.
func Summary(report Report) string {
	return fmt.Sprintf(
		"node %s: %d of %d block devices free, %d online CPUs over %d cores (hyperthreading %v), "+
			"%d MiB of huge pages, %d interfaces, %d NVMe controllers taken by a userspace "+
			"driver, %d readings unavailable",
		report.Node,
		len(report.AvailableDevices()), len(report.Devices),
		report.CPU.OnlineCPUs, report.CPU.PhysicalCores, report.CPU.HyperThreading,
		report.HugePageBytes()>>20,
		len(report.Interfaces),
		len(report.ControllersTakenByUserspace()),
		len(report.Unreadable),
	)
}
