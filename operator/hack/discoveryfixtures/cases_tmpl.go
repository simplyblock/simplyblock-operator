// The document-shape cases, §13 of the document, and the table every family is
// collected into.
//
// A template case is about the half of a draft no probe reports: the cluster
// block, its name, and the fields a reviewer is being asked to approve without
// any reading behind them. The fleet under each is therefore the plainest one
// available, so that nothing in the document distracts from the block itself.

package main

import (
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

func templateCases() map[string]Case {
	plain := func() []nodeprobe.Report {
		return fleet(3, func(_ int, name string) nodeprobe.Report {
			return host(name, cpu(1, 16, 2), disks(layoutA.disks(3*tb)...))
		})
	}
	with := func(spec *simplyblockv1alpha2.DiscoverSpec) Case {
		reports := plain()
		return Case{Family: "tmpl", Reports: reports, Nodes: kubeFleet(reports), Discover: spec}
	}

	// A two-socket fleet, which is where the absent socket layout costs a
	// cluster the second half of every worker.
	twoSocket := fleet(3, func(_ int, name string) nodeprobe.Report {
		return host(name, disks(spread(3*tb, 2, 2)...))
	})

	cases := map[string]Case{
		"TMPL-01": with(nil),
		"TMPL-02": with(nil),
		"TMPL-03": with(nil),
		"TMPL-05": with(&simplyblockv1alpha2.DiscoverSpec{ClusterRef: "existing-cluster"}),
		"TMPL-06": {Family: "tmpl", Reports: twoSocket, Nodes: kubeFleet(twoSocket)},
		"TMPL-07": with(nil),
		"TMPL-08": with(&simplyblockv1alpha2.DiscoverSpec{
			DeviceFilter: &simplyblockv1alpha2.DeviceFilter{
				PcieDenyList:   []string{"0000:b0:00.0"},
				DriveSizeRange: "1T-8T",
			},
		}),
	}

	// A run whose name pushes the cluster it proposes past what a
	// StorageCluster name may be.
	cases["TMPL-04"] = with(&simplyblockv1alpha2.DiscoverSpec{
		ConfigName: "discovered-the-quarterly-storage-expansion-for-the-frankfurt-racks",
	})

	slugs := map[string]string{
		"TMPL-01": "every-draft-formats-its-drives",
		"TMPL-02": "the-subsystem-count-is-not-a-reading",
		"TMPL-03": "a-generated-config-name",
		"TMPL-04": "a-cluster-name-past-its-limit",
		"TMPL-05": "a-growth-document",
		"TMPL-06": "a-two-socket-fleet-and-no-socket-layout",
		"TMPL-07": "the-environment-is-copied-through",
		"TMPL-08": "the-filter-is-resolved-and-not-carried",
	}
	gaps := map[string]string{"TMPL-04": "G-11", "TMPL-06": "G-12"}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		entry.Gap = gaps[id]
		cases[id] = entry
	}

	// The environment case says what it varies, since the value is on the run
	// rather than in the fleet.
	openshift := cases["TMPL-07"]
	openshift.Environment = simplyblockv1alpha2.KubernetesEnvironmentOpenShift
	cases["TMPL-07"] = openshift

	return cases
}

// allCases is every family's table, merged.
//
// A case identifier that two families claim is a mistake worth stopping for:
// the second would silently overwrite the first and one of the two directories
// would never be written.
func allCases() map[string]Case {
	out := map[string]Case{}
	for _, family := range []map[string]Case{
		devCases(), numaCases(), sizeCases(), pciCases(), fleetCases(),
		netCases(), roleCases(), filterCases(), heldCases(), failCases(),
		configMapCases(), templateCases(),
	} {
		for id, c := range family {
			if _, repeated := out[id]; repeated {
				panic("two families claim " + id)
			}
			out[id] = c
		}
	}
	return out
}
