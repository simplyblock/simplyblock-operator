// The node-role cases, §8 of the document, and the filter cases, §9.
//
// A role is a label rather than a taint, and the two do not coincide:
// Kubernetes taints its control-plane nodes and OpenShift usually does not taint
// its infrastructure ones. The role cases are therefore about node objects
// rather than about reports, and every worker in them carries the same disks.
//
// The filter cases are the opposite shape: one worker with a deliberately mixed
// set of disks, and a different filter on each run.

package main

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// sameDisks is n workers that agree on everything, so that a role case's groups
// differ only by what the node objects say.
func sameDisks(n int) []nodeprobe.Report {
	return fleet(n, func(_ int, name string) nodeprobe.Report {
		return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
	})
}

func roleCases() map[string]Case {
	three := sameDisks(3)
	six := sameDisks(6)

	// Three infrastructure nodes and three plain workers, identical hardware.
	tiered := make([]corev1.Node, 0, len(six))
	for i, report := range six {
		opts := []nodeOpt{reachableAt(managementAddress(report.Node))}
		if i < 3 {
			opts = append(opts, role("infra"))
		}
		tiered = append(tiered, kubeNode(report.Node, opts...))
	}

	// A control-plane node with free disks beside two workers.
	mixedRoles := []corev1.Node{
		kubeNode(three[0].Node, reachableAt(managementAddress(three[0].Node)),
			role("control-plane"), tainted("node-role.kubernetes.io/control-plane", "",
				corev1.TaintEffectNoSchedule)),
		kubeNode(three[1].Node, reachableAt(managementAddress(three[1].Node))),
		kubeNode(three[2].Node, reachableAt(managementAddress(three[2].Node))),
	}

	// Thirty-two machines across the three tiers.
	large := sameDisks(32)
	spreadRoles := make([]corev1.Node, 0, len(large))
	for i, report := range large {
		opts := []nodeOpt{reachableAt(managementAddress(report.Node))}
		switch {
		case i < 4:
			opts = append(opts, role("infra"))
		case i < 7:
			opts = append(opts, role("control-plane"))
		default:
			opts = append(opts, role("worker"))
		}
		spreadRoles = append(spreadRoles, kubeNode(report.Node, opts...))
	}

	withControlPlane := &simplyblockv1alpha2.DiscoverSpec{
		EnableControlPlaneNodes: ptr.To(true),
	}

	cases := map[string]Case{
		"ROLE-01": {Family: "role", Reports: three, Nodes: kubeFleet(three)},
		"ROLE-02": {Family: "role", Reports: six, Nodes: tiered},
		"ROLE-03": {Family: "role", Reports: three, Nodes: mixedRoles, Discover: withControlPlane},
		"ROLE-04": {Family: "role", Reports: three[:1],
			Nodes: []corev1.Node{kubeNode(three[0].Node,
				reachableAt(managementAddress(three[0].Node)), role("infra"), role("worker"))}},
		"ROLE-05": {Family: "role", Reports: three[:1], Discover: withControlPlane,
			Nodes: []corev1.Node{kubeNode(three[0].Node,
				reachableAt(managementAddress(three[0].Node)), role("control-plane"), role("worker"))}},
		"ROLE-06": {Family: "role", Reports: three[:1], Discover: withControlPlane,
			Nodes: []corev1.Node{kubeNode(three[0].Node,
				reachableAt(managementAddress(three[0].Node)), role("master"))}},
		"ROLE-08": {Family: "role", Reports: three[:2],
			Nodes: []corev1.Node{
				kubeNode(three[0].Node, reachableAt(managementAddress(three[0].Node)), cordoned()),
				kubeNode(three[1].Node, reachableAt(managementAddress(three[1].Node)),
					tainted("storage", "drained", corev1.TaintEffectNoExecute)),
			}},
		"ROLE-09": {Family: "role", Reports: three[:1],
			Nodes: []corev1.Node{kubeNode(three[0].Node,
				reachableAt(managementAddress(three[0].Node)), role("database"))}},
		"ROLE-10": {Family: "role", Reports: large, Nodes: spreadRoles, Discover: withControlPlane},

		// The seam case: a plan built with no node objects at all, which the
		// controller never does because it always lists them.
		"ROLE-07": {Family: "role", Slug: "a-plan-with-no-node-objects",
			Note: "Driven in the discovery package with a Planner carrying no KubeNodes, because the " +
				"controller always reads the node objects for the workers its run settled on."},
	}

	slugs := map[string]string{
		"ROLE-01": "workers-with-no-role-label",
		"ROLE-02": "an-infrastructure-tier-beside-the-workers",
		"ROLE-03": "a-control-plane-node-with-disks",
		"ROLE-04": "a-node-labeled-infra-and-worker",
		"ROLE-05": "a-node-labeled-control-plane-and-worker",
		"ROLE-06": "the-master-spelling",
		"ROLE-08": "a-cordoned-node-and-a-tainted-one",
		"ROLE-09": "a-role-this-product-does-not-know",
		"ROLE-10": "thirty-two-machines-across-three-tiers",
	}
	gaps := map[string]string{"ROLE-08": "G-8"}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		entry.Gap = gaps[id]
		cases[id] = entry
	}
	return cases
}

// mixedDisks is the worker every filter case is run against: four disks of two
// models and three sizes, one of them partitioned and one mounted, so that each
// filter has something to take and something to leave.
func mixedDisks() []nodeprobe.Device {
	return []nodeprobe.Device{
		nvme("nvme0n1", "0000:5e:00.0", 0, 2*tb),
		nvme("nvme1n1", "0000:5f:00.0", 0, 2*tb, modeled("INTEL SSDPF2KX038TZ")),
		nvme("nvme2n1", "0000:af:00.0", 0, 512*gb),
		nvme("nvme3n1", "0000:b0:00.0", 0, 2*tb, refused(blockdev.ReasonPartitioned)),
		nvme("nvme4n1", "0000:b1:00.0", 0, 2*tb,
			refused(blockdev.ReasonPartitioned, blockdev.ReasonMounted)),
	}
}

// filtered is a run carrying one device filter.
func filtered(edit func(*simplyblockv1alpha2.DeviceFilter)) *simplyblockv1alpha2.DiscoverSpec {
	filter := &simplyblockv1alpha2.DeviceFilter{}
	edit(filter)
	return &simplyblockv1alpha2.DiscoverSpec{DeviceFilter: filter}
}

func filterCases() map[string]Case {
	nvmeWorker := func() []nodeprobe.Report {
		return []nodeprobe.Report{host("worker-01", cpu(1, 16, 2), disks(mixedDisks()...))}
	}
	blockWorker := func() []nodeprobe.Report {
		return []nodeprobe.Report{host("worker-01", cpu(1, 16, 2), disks(
			blk("sda", 0, 512*gb),
			blk("sdb", 0, 2*tb),
			blk("sdc", 0, 2*tb),
			blk("vdb", 0, 4*tb),
			blk("vdc", 0, 4*tb),
			blk("vdd", 0, 4*tb),
		))}
	}

	with := func(reports []nodeprobe.Report, spec *simplyblockv1alpha2.DiscoverSpec) Case {
		return Case{Family: "filt", Reports: reports, Nodes: kubeFleet(reports), Discover: spec}
	}
	block := func(edit func(*simplyblockv1alpha2.DeviceFilter)) *simplyblockv1alpha2.DiscoverSpec {
		return filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.EnableLogicalBlockDevices = ptr.To(true)
			edit(f)
		})
	}

	cases := map[string]Case{
		"FILT-01": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.PcieDenyList = []string{"0000:5e:00.0"}
		})),
		"FILT-02": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.PcieAllowList = []string{"0000:5e:00.0", "0000:5f:00.0"}
		})),
		"FILT-03": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.PcieModel = "MZQL2"
		})),
		"FILT-04": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.DriveSizeRange = "1T-4T"
		})),
		"FILT-05": with([]nodeprobe.Report{host("worker-01", cpu(1, 16, 2), disks(
			nvme("nvme0n1", "0000:5e:00.0", 0, 2*tb),
			nvme("nvme1n1", "0000:5f:00.0", 0, 2*tb),
			nvme("nvme2n1", "0000:af:00.0", 0, 1920*gb),
		))}, filtered(func(f *simplyblockv1alpha2.DeviceFilter) { f.DriveSizeRange = "2T" })),
		"FILT-06": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.DriveSizeRange = "2T-1T"
		})),
		"FILT-07": with(blockWorker(), block(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.BlockDenyList = []string{"/dev/sda"}
		})),
		"FILT-08": with(blockWorker(), block(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.BlockAllowList = []string{"/dev/vdb", "/dev/vdc"}
		})),
		"FILT-09": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.EnablePartitionedDevices = ptr.To(true)
		})),
		"FILT-10": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.PcieAllowList = []string{"0000:ff:00.0"}
		})),
		"FILT-11": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.PcieAllowList = []string{"0000:5E:00.0", "0000:5F:00.0"}
		})),
		"FILT-12": with(nvmeWorker(), filtered(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.PcieAllowList = []string{"0000:5e:00.0", "0000:5f:00.0"}
			f.PcieDenyList = []string{"0000:5e:00.0"}
		})),
		"FILT-13": with(blockWorker(), block(func(f *simplyblockv1alpha2.DeviceFilter) {
			f.DriveSizeRange = "3T-8T"
		})),
		"FILT-14": with(nvmeWorker(), nil),
	}

	slugs := map[string]string{
		"FILT-01": "a-pci-deny-list",
		"FILT-02": "a-pci-allow-list",
		"FILT-03": "a-model-substring",
		"FILT-04": "a-size-range",
		"FILT-05": "a-bare-size-is-both-bounds",
		"FILT-06": "a-size-range-that-counts-backward",
		"FILT-07": "a-block-deny-list",
		"FILT-08": "a-block-allow-list",
		"FILT-09": "a-partition-table-waived",
		"FILT-10": "a-filter-that-matches-nothing",
		"FILT-11": "an-allow-list-in-uppercase",
		"FILT-12": "one-address-allowed-and-denied",
		"FILT-13": "a-size-range-on-a-block-run",
		"FILT-14": "no-filter-at-all",
	}
	gaps := map[string]string{"FILT-06": "G-9"}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		entry.Gap = gaps[id]
		cases[id] = entry
	}
	return cases
}
