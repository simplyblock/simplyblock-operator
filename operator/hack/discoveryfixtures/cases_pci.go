// The PCI addressing and fleet-grouping cases, §5 and §6 of the document.
//
// A group's device selection is shared by every worker in it, so grouping is a
// statement about what the machines have rather than a presentation choice.
// These cases vary how much of a fleet agrees: all of it, none of it, and the
// partial agreements in between that are what a real fleet looks like after a
// few years of replacements.

package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// layout is one arrangement of slots, which is what two workers have to agree
// on to share a group.
type layout []string

var (
	layoutA = layout{"0000:5e:00.0", "0000:5f:00.0", "0000:af:00.0", "0000:b0:00.0"}
	layoutB = layout{"0000:3b:00.0", "0000:3c:00.0", "0000:d8:00.0", "0000:d9:00.0"}
)

// distinct is a layout nothing else in the fleet shares, keyed off the worker's
// number so that every such worker differs from every other.
func distinct(index int) layout {
	return layout{
		fmt.Sprintf("0000:%02x:00.0", 0x10+index),
		fmt.Sprintf("0000:%02x:00.1", 0x10+index),
	}
}

// at builds the disks of a layout, all on memory node 0 and all the same size.
func (l layout) disks(size uint64) []nodeprobe.Device {
	out := make([]nodeprobe.Device, 0, len(l))
	for i, address := range l {
		out = append(out, nvme(fmt.Sprintf("nvme%dn1", i), address, 0, size))
	}
	return out
}

// workersOn is n workers all handing over the same layout.
func workersOn(n int, l layout) []nodeprobe.Report {
	return fleet(n, func(_ int, name string) nodeprobe.Report {
		return host(name, cpu(1, 16, 2), disks(l.disks(3*tb)...))
	})
}

func pciCases() map[string]Case {
	withNodes := func(reports []nodeprobe.Report) Case {
		return Case{Family: "pci", Reports: reports, Nodes: kubeFleet(reports)}
	}

	// Sixteen on one layout and sixteen on another.
	twoLayouts := fleet(32, func(index int, name string) nodeprobe.Report {
		chosen := layoutA
		if index > 16 {
			chosen = layoutB
		}
		return host(name, cpu(1, 16, 2), disks(chosen.disks(3*tb)...))
	})

	// Twenty on one, six on another, and six that agree with nobody: the shape
	// of a fleet nobody bought all at once.
	stragglers := fleet(32, func(index int, name string) nodeprobe.Report {
		switch {
		case index <= 20:
			return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
		case index <= 26:
			return host(name, cpu(1, 16, 2), disks(layoutB.disks(3*tb)...))
		default:
			return host(name, cpu(1, 16, 2), disks(distinct(index).disks(3*tb)...))
		}
	})

	// Eight identical machines but for one disk of one of them in another slot.
	oddOneOut := fleet(8, func(index int, name string) nodeprobe.Report {
		chosen := layoutA
		if index == 5 {
			chosen = layout{"0000:5e:00.0", "0000:5f:00.0", "0000:af:00.0", "0000:c8:00.0"}
		}
		return host(name, cpu(1, 16, 2), disks(chosen.disks(3*tb)...))
	})

	// Three on one layout, two agreeing with nobody.
	partial := fleet(5, func(index int, name string) nodeprobe.Report {
		chosen := layoutA
		if index > 3 {
			chosen = distinct(index)
		}
		return host(name, cpu(1, 16, 2), disks(chosen.disks(3*tb)...))
	})

	cases := map[string]Case{
		"PCI-01": withNodes(workersOn(32, layoutA)),
		"PCI-02": withNodes([]nodeprobe.Report{
			host("worker-01", cpu(1, 16, 2), disks(layoutA.disks(3*tb)...)),
			host("worker-02", cpu(1, 16, 2), disks(layoutB.disks(3*tb)...)),
		}),
		"PCI-03": withNodes(twoLayouts),
		"PCI-04": withNodes(fleet(32, func(index int, name string) nodeprobe.Report {
			return host(name, cpu(1, 16, 2), disks(distinct(index).disks(3*tb)...))
		})),
		"PCI-05": withNodes([]nodeprobe.Report{host("worker-01", cpu(1, 16, 2), disks(
			nvme("nvme3n1", "0000:b0:00.0", 0, 3*tb),
			nvme("nvme2n1", "0000:af:00.0", 0, 3*tb),
			nvme("nvme1n1", "0000:5f:00.0", 0, 3*tb),
			nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
		))}),
		"PCI-06": withNodes([]nodeprobe.Report{host("worker-01", cpu(1, 16, 2), disks(
			layout{
				"0000:5e:00.0", "0000:5e:00.1", "0000:5f:00.0", "0000:5f:00.1", "0000:60:00.0",
				"0000:af:00.0", "0000:af:00.1", "0000:b0:00.0", "0000:b0:00.1", "0000:b1:00.0",
			}.disks(3*tb)...))}),
		"PCI-07": withNodes([]nodeprobe.Report{host("worker-01", cpu(1, 16, 2), disks(
			nvme("nvme0n1", "10000:01:00.0", 0, 3*tb),
			nvme("nvme1n1", "10000:02:00.0", 0, 3*tb),
		))}),
		"PCI-08": withNodes([]nodeprobe.Report{
			host("worker-01", cpu(1, 16, 2), disks(
				nvme("nvme0n1", "0000:5E:00.0", 0, 3*tb),
				nvme("nvme1n1", "0000:5F:00.0", 0, 3*tb),
			)),
			host("worker-02", cpu(1, 16, 2), disks(
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb),
				nvme("nvme1n1", "0000:5f:00.0", 0, 3*tb),
			)),
		}),
		"PCI-10": withNodes(partial),
		"PCI-11": withNodes(stragglers),
		"PCI-12": withNodes(oddOneOut),
		"PCI-13": withNodes([]nodeprobe.Report{
			host("worker-01", cpu(1, 16, 2), disks(layoutA[:2].disks(2*tb)...)),
			host("worker-02", cpu(1, 16, 2), disks(layoutA[:2].disks(4*tb)...)),
		}),
		"PCI-14": withNodes([]nodeprobe.Report{
			host("worker-01", cpu(1, 16, 2), disks(layoutA.disks(3*tb)...)),
			host("worker-02", cpu(1, 16, 2), disks(layoutA.disks(3*tb)...)),
			host("worker-03", cpu(1, 16, 2), disks(layoutA[:3].disks(3*tb)...)),
		}),
	}

	// Unpadded names, which sort worker-1, worker-10, worker-11 and decide the
	// order the groups are numbered in.
	unpadded := make([]nodeprobe.Report, 0, 32)
	for i := 1; i <= 32; i++ {
		chosen := layoutA
		if i > 16 {
			chosen = layoutB
		}
		unpadded = append(unpadded,
			host(fmt.Sprintf("worker-%d", i), cpu(1, 16, 2), disks(chosen.disks(3*tb)...)))
	}
	cases["PCI-09"] = withNodes(unpadded)

	slugs := map[string]string{
		"PCI-01": "a-fleet-that-agrees",
		"PCI-02": "two-workers-that-do-not",
		"PCI-03": "sixteen-and-sixteen",
		"PCI-04": "every-worker-distinct",
		"PCI-05": "addresses-reported-descending",
		"PCI-06": "ten-disks-across-two-buses",
		"PCI-07": "a-five-digit-pci-domain",
		"PCI-08": "uppercase-hex-against-lowercase",
		"PCI-09": "unpadded-worker-names",
		"PCI-10": "three-agree-and-two-do-not",
		"PCI-11": "a-majority-layout-with-stragglers",
		"PCI-12": "one-slot-moved-on-one-worker",
		"PCI-13": "one-layout-two-capacities",
		"PCI-14": "a-worker-holding-a-subset",
	}
	gaps := map[string]string{"PCI-07": "G-6", "PCI-13": "G-13"}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		entry.Gap = gaps[id]
		cases[id] = entry
	}
	return cases
}

func fleetCases() map[string]Case {
	withNodes := func(reports []nodeprobe.Report) Case {
		return Case{Family: "fleet", Reports: reports, Nodes: kubeFleet(reports)}
	}

	uniform32 := workersOn(32, layoutA)

	// Three workers of the thirty-two have nothing the rules would take.
	someRefused := fleet(32, func(index int, name string) nodeprobe.Report {
		if index%11 == 0 {
			return host(name, cpu(1, 16, 2), disks(
				nvme("nvme0n1", "0000:5e:00.0", 0, 3*tb, refused(blockdev.ReasonMounted)),
				nvme("nvme1n1", "0000:5f:00.0", 0, 3*tb, refused(blockdev.ReasonMounted)),
			))
		}
		return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
	})

	// The same fleet with the reports reversed, which is what a different
	// listing order looks like from the operator's side.
	reversed := make([]nodeprobe.Report, 0, len(uniform32))
	for i := len(uniform32) - 1; i >= 0; i-- {
		reversed = append(reversed, uniform32[i])
	}

	cases := map[string]Case{
		"FLEET-01": withNodes(workersOn(1, layoutA)),
		"FLEET-02": withNodes(workersOn(3, layoutA)),
		"FLEET-03": withNodes(uniform32),
		"FLEET-05": withNodes(someRefused),
		"FLEET-06": withNodes(reversed),
	}

	// A run whose probes wrote nothing at all.
	cases["FLEET-04"] = Case{
		Family: "fleet", Workers: []string{"worker-01", "worker-02", "worker-03"},
		Nodes: []corev1.Node{
			kubeNode("worker-01", reachableAt("10.10.10.1")),
			kubeNode("worker-02", reachableAt("10.10.10.2")),
			kubeNode("worker-03", reachableAt("10.10.10.3")),
		},
	}

	// Two ConfigMaps carrying a report for one node, which is what a probe
	// restarted under a second object name leaves behind.
	doubled := workersOn(2, layoutA)
	cases["FLEET-07"] = Case{
		Family: "fleet", Reports: doubled, Nodes: kubeFleet(doubled),
		Amend: func(run string, maps []*corev1.ConfigMap) []*corev1.ConfigMap {
			copied := maps[0].DeepCopy()
			copied.Name += "-again"
			return append(maps, copied)
		},
	}

	// A report for a machine this run is not about.
	foreign := workersOn(3, layoutA)
	cases["FLEET-08"] = Case{
		Family: "fleet", Reports: foreign, Nodes: kubeFleet(foreign),
		Workers: []string{"worker-01", "worker-02"},
	}

	slugs := map[string]string{
		"FLEET-01": "one-worker",
		"FLEET-02": "three-uniform-workers",
		"FLEET-03": "thirty-two-uniform-workers",
		"FLEET-04": "no-reports-at-all",
		"FLEET-05": "three-of-thirty-two-refused",
		"FLEET-06": "the-same-fleet-in-another-order",
		"FLEET-07": "two-reports-for-one-worker",
		"FLEET-08": "a-report-this-run-is-not-about",
	}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		cases[id] = entry
	}
	return cases
}
