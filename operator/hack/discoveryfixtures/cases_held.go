// The cases for devices the probe declined and controllers a userspace driver
// holds, §10 of the document, the refusal cases of §11, the report-validity
// cases of §12, and the document-shape cases of §13.
//
// The four sections share a shape: each is a fleet the run either cannot build
// a draft from or can only partly, and what is being checked is the account the
// run gives of itself rather than the document it writes.

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/atlas/pci"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// idleControllers is n NVMe controllers a userspace driver holds and nothing is
// driving, which is what a machine that has run this product before presents.
func idleControllers(n int, driver string) []nodeprobe.Controller {
	out := make([]nodeprobe.Controller, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, controller(fmt.Sprintf("0000:%02x:00.0", 0x5e+i), driver))
	}
	return out
}

func heldCases() map[string]Case {
	one := func(report nodeprobe.Report) Case {
		return Case{Family: "held", Reports: []nodeprobe.Report{report},
			Nodes: kubeFleet([]nodeprobe.Report{report})}
	}
	allMounted := func() []nodeprobe.Device {
		out := layoutA.disks(3 * tb)
		for i := range out {
			refused(blockdev.ReasonMounted)(&out[i])
		}
		return out
	}
	allHeld := func() []nodeprobe.Device {
		out := layoutA.disks(3 * tb)
		for i := range out {
			refused(blockdev.ReasonStacked)(&out[i])
		}
		return out
	}

	busy := idleControllers(4, pci.DriverUIOGeneric)
	for i := range busy {
		busy[i].InUse = ptr.To(true)
	}
	halfBusy := idleControllers(4, pci.DriverUIOGeneric)
	halfBusy[2].InUse, halfBusy[3].InUse = ptr.To(true), ptr.To(true)

	// A controller the probe could not check: the answer is absent rather than
	// false, so nothing may claim it.
	unchecked := idleControllers(2, pci.DriverUIOGeneric)
	for i := range unchecked {
		unchecked[i].InUse = nil
	}

	loopbacks := make([]nodeprobe.Device, 0, 16)
	for i := 0; i < 16; i++ {
		loopbacks = append(loopbacks, blk(fmt.Sprintf("loop%d", i), 64*gb, looped()))
	}

	cases := map[string]Case{
		"HELD-01": one(host("worker-01", cpu(1, 16, 2), disks(allMounted()...))),
		"HELD-02": one(host("worker-01", cpu(1, 16, 2), disks(allHeld()...))),
		"HELD-03": one(host("worker-01", cpu(1, 16, 2),
			controllers(idleControllers(4, pci.DriverUIOGeneric)...))),
		"HELD-04": one(host("worker-01", cpu(1, 16, 2), controllers(busy...))),
		"HELD-05": one(host("worker-01", cpu(1, 16, 2),
			disks(
				nvme("nvme0n1", "0000:af:00.0", 0, 3*tb),
				nvme("nvme1n1", "0000:b0:00.0", 0, 3*tb),
			),
			controllers(
				controller("0000:5e:00.0", pci.DriverUIOGeneric),
				controller("0000:5f:00.0", pci.DriverUIOGeneric),
				controller("0000:af:00.0", "nvme"),
				controller("0000:b0:00.0", "nvme"),
			))),
		"HELD-06": one(host("worker-01", cpu(1, 16, 2),
			controllers(idleControllers(4, pci.DriverVFIO)...))),
		"HELD-07": one(host("worker-01", cpu(1, 16, 2),
			disks(layoutA.disks(3*tb)...),
			controllers(
				controller("0000:5e:00.0", "nvme"),
				controller("0000:5f:00.0", "nvme"),
				controller("0000:af:00.0", "nvme"),
				controller("0000:b0:00.0", "nvme"),
			))),
		"HELD-09": one(host("worker-01", cpu(1, 16, 2),
			disks(append(loopbacks, layoutA.disks(3*tb)...)...))),
		"HELD-10": one(host("worker-01", cpu(1, 16, 2), controllers(halfBusy...))),
		"HELD-11": one(host("worker-01", cpu(1, 16, 2), controllers(unchecked...),
			unreadable("read the process table: permission denied"))),
	}

	// The same idle controllers on a run that scans logical block devices,
	// which names a device by a path a controller has none of.
	cases["HELD-08"] = Case{
		Family:   "held",
		Discover: blockClass(),
		Reports: []nodeprobe.Report{host("worker-01", cpu(1, 16, 2),
			controllers(idleControllers(4, pci.DriverUIOGeneric)...))},
		Nodes: []corev1.Node{kubeNode("worker-01", reachableAt(managementAddress("worker-01")))},
	}

	slugs := map[string]string{
		"HELD-01": "every-disk-mounted",
		"HELD-02": "every-disk-in-a-device-mapper-stack",
		"HELD-03": "four-idle-userspace-controllers",
		"HELD-04": "four-userspace-controllers-in-use",
		"HELD-05": "idle-controllers-beside-kernel-disks",
		"HELD-06": "an-idle-controller-on-vfio-pci",
		"HELD-07": "kernel-controllers-already-presented",
		"HELD-08": "idle-controllers-on-a-block-run",
		"HELD-09": "sixteen-loopbacks-beside-four-disks",
		"HELD-10": "half-the-controllers-in-use",
		"HELD-11": "controllers-nothing-could-check",
	}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		cases[id] = entry
	}
	return cases
}

func failCases() map[string]Case {
	bare := func(n int, build func(name string) nodeprobe.Report) Case {
		reports := fleet(n, func(_ int, name string) nodeprobe.Report { return build(name) })
		return Case{Family: "fail", Reports: reports, Nodes: kubeFleet(reports)}
	}

	mounted := func(name string) nodeprobe.Report {
		out := layoutA.disks(3 * tb)
		for i := range out {
			refused(blockdev.ReasonMounted)(&out[i])
		}
		return host(name, cpu(1, 16, 2), disks(out...))
	}
	partitions := func(name string) nodeprobe.Report {
		return host(name, cpu(1, 16, 2), disks(
			nvme("nvme0n1p1", "0000:5e:00.0", 0, 512*gb, partOf()),
			nvme("nvme0n1p2", "0000:5e:00.0", 0, 512*gb, partOf()),
		))
	}

	cases := map[string]Case{
		"FAIL-01": bare(3, func(name string) nodeprobe.Report {
			return host(name, cpu(1, 16, 2))
		}),
		"FAIL-02": bare(3, mounted),
		"FAIL-06": bare(3, func(name string) nodeprobe.Report {
			return host(name, cpu(1, 2, 1), mem(16*gb, 14*gb, 16*gb),
				disks(layoutA.disks(3*tb)...))
		}),
		"FAIL-07": bare(3, func(name string) nodeprobe.Report {
			return host(name, cpu(1, 4, 2), mem(4*gb, 900*mb, 4*gb),
				disks(layoutA.disks(3*tb)...))
		}),
		"FAIL-10": bare(1, partitions),
	}

	// Every report unreadable, which is a probe that ran and wrote nothing a
	// reader can use.
	broken := fleet(3, func(_ int, name string) nodeprobe.Report {
		return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
	})
	cases["FAIL-03"] = Case{
		Family: "fail", Reports: broken, Nodes: kubeFleet(broken),
		Amend: func(_ string, maps []*corev1.ConfigMap) []*corev1.ConfigMap {
			for _, cm := range maps {
				cm.Data[nodeprobe.ReportKey] = "{ this is not a report"
			}
			return maps
		},
	}

	// A filter that excludes every disk in the fleet.
	excluded := fleet(3, func(_ int, name string) nodeprobe.Report {
		return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
	})
	cases["FAIL-04"] = Case{
		Family: "fail", Reports: excluded, Nodes: kubeFleet(excluded),
		Discover: filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.DriveSizeRange = "100T-200T"
		}),
	}

	// Disks everywhere and no interface anything reaches the machines on.
	unreachable := fleet(3, func(_ int, name string) nodeprobe.Report {
		return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...), ifaces(
			nic("cni0", inventory.LinkBridge, holding("10.42.2.1")),
			nic("lo", inventory.LinkLoopback, holding("127.0.0.1")),
		))
	})
	unreachableNodes := make([]corev1.Node, 0, len(unreachable))
	for _, report := range unreachable {
		unreachableNodes = append(unreachableNodes, kubeNode(report.Node))
	}
	cases["FAIL-05"] = Case{Family: "fail", Reports: unreachable, Nodes: unreachableNodes}

	// A worker whose processor tree the probe could not read, with everything
	// else in order.
	blind := host("worker-01", noCPUTopology(), disks(layoutA.disks(3*tb)...),
		unreadable("read /sys/devices/system/cpu: permission denied"))
	cases["FAIL-08"] = Case{Family: "fail", Reports: []nodeprobe.Report{blind},
		Nodes: kubeFleet([]nodeprobe.Report{blind})}

	// One worker of thirty-two refused, which is an event rather than a
	// failure.
	nearlyAll := fleet(32, func(index int, name string) nodeprobe.Report {
		if index == 17 {
			return mounted(name)
		}
		return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
	})
	cases["FAIL-09"] = Case{Family: "fail", Reports: nearlyAll, Nodes: kubeFleet(nearlyAll)}

	slugs := map[string]string{
		"FAIL-01": "no-devices-and-no-controllers",
		"FAIL-02": "every-disk-in-the-fleet-mounted",
		"FAIL-03": "every-report-unreadable",
		"FAIL-04": "a-filter-that-excludes-the-fleet",
		"FAIL-05": "no-usable-interface-anywhere",
		"FAIL-06": "every-worker-under-the-vcpu-floor",
		"FAIL-07": "every-worker-at-four-gibibytes",
		"FAIL-08": "an-unreadable-processor-tree",
		"FAIL-09": "one-worker-of-thirty-two-refused",
		"FAIL-10": "a-worker-whose-disks-are-partitions",
	}
	gaps := map[string]string{"FAIL-05": "G-7", "FAIL-06": "G-10", "FAIL-07": "G-4"}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		entry.Gap = gaps[id]
		cases[id] = entry
	}
	return cases
}

func configMapCases() map[string]Case {
	three := fleet(3, func(_ int, name string) nodeprobe.Report {
		return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
	})
	withNodes := func(reports []nodeprobe.Report, amend func(string, []*corev1.ConfigMap) []*corev1.ConfigMap) Case {
		return Case{Family: "cm", Reports: reports, Nodes: kubeFleet(reports), Amend: amend}
	}

	// A report written by the probe of the previous schema, which named no
	// interface kind and therefore described its bonds as virtual and nothing
	// else.
	previousVersion := func(_ string, maps []*corev1.ConfigMap) []*corev1.ConfigMap {
		var report map[string]any
		_ = json.Unmarshal([]byte(maps[0].Data[nodeprobe.ReportKey]), &report)
		report["version"] = nodeprobe.ReportVersion - 1
		rewritten, _ := json.MarshalIndent(report, "", "  ")
		maps[0].Data[nodeprobe.ReportKey] = string(rewritten)
		return maps
	}

	cases := map[string]Case{
		"CM-01": withNodes(three, previousVersion),
		"CM-02": withNodes(three, func(_ string, maps []*corev1.ConfigMap) []*corev1.ConfigMap {
			delete(maps[0].Data, nodeprobe.ReportKey)
			return maps
		}),
		"CM-03": withNodes(three, func(_ string, maps []*corev1.ConfigMap) []*corev1.ConfigMap {
			maps[0].Data[nodeprobe.ReportKey] = `{"version": 4, "node": "worker-01",`
			return maps
		}),
		"CM-04": withNodes(three, func(_ string, maps []*corev1.ConfigMap) []*corev1.ConfigMap {
			maps[0].Data[nodeprobe.ReportKey] = strings.Replace(
				maps[0].Data[nodeprobe.ReportKey], `"node": "worker-01"`, `"node": ""`, 1)
			return maps
		}),
		"CM-05": withNodes(three, func(_ string, maps []*corev1.ConfigMap) []*corev1.ConfigMap {
			delete(maps[0].Labels, nodeprobe.LabelRun)
			return maps
		}),
	}

	// A worker whose name is longer than a label value may be, so that the
	// label is truncated and the report inside it is not.
	const long = "ip-10-0-1-23.eu-central-1.compute.internal.a-very-long-suffix-nobody-shortened"
	longNamed := host(long, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
	cases["CM-06"] = Case{
		Family: "cm", Reports: []nodeprobe.Report{longNamed},
		Nodes: []corev1.Node{kubeNode(long, reachableAt("10.10.10.1"))},
	}

	// A report of a worker with as many devices as the schema will hold, which
	// is the largest object a probe writes.
	large := host("worker-01", cpu(1, 16, 2), disks(manyNVMe(128, 512*gb)...))
	cases["CM-07"] = Case{
		Family: "cm", Reports: []nodeprobe.Report{large},
		Nodes: kubeFleet([]nodeprobe.Report{large}),
	}

	slugs := map[string]string{
		"CM-01": "a-report-from-the-previous-schema",
		"CM-02": "a-configmap-with-no-report-key",
		"CM-03": "a-report-that-does-not-parse",
		"CM-04": "a-report-naming-no-node",
		"CM-05": "a-configmap-without-the-run-label",
		"CM-06": "a-worker-name-longer-than-a-label",
		"CM-07": "a-report-of-a-hundred-and-twenty-eight-devices",
	}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		cases[id] = entry
	}
	return cases
}
