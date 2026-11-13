// The network-attachment cases, the end of §7 of the document: a run that names
// Multus NetworkAttachmentDefinitions for its storage nodes instead of host
// networking.
//
// Every worker is the ordinary host, with eth0 and eth1, and reports the
// namespace's definitions the way a probe in a cluster running Multus does.

package main

import (
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// The definitions the namespace holds: a macvlan on the management NIC and an
// ipvlan on the data NIC.
var (
	storageMgmt = nodeprobe.NetworkAttachment{
		Namespace: "simplyblock", Name: "storage-mgmt",
		Type: "macvlan", HostInterface: "eth0", Mode: "bridge", IPAM: "whereabouts",
	}
	storageData = nodeprobe.NetworkAttachment{
		Namespace: "simplyblock", Name: "storage-data",
		Type: "ipvlan", HostInterface: "eth1", Mode: "l2", IPAM: "static",
	}
)

// definitions is a worker reporting the namespace's definitions in the state
// given.
func definitions(state string, held ...nodeprobe.NetworkAttachment) hostOpt {
	return func(report *nodeprobe.Report) {
		report.NetworkAttachments = &nodeprobe.NetworkAttachments{State: state, Namespace: "simplyblock", Definitions: held}
	}
}

// withoutData drops eth1 from a worker.
func withoutData() hostOpt {
	return func(report *nodeprobe.Report) {
		report.Interfaces = report.Interfaces[:1]
	}
}

// attachedThrough is a run naming the definitions given.
func attachedThrough(management, data string) *simplyblockv1alpha2.DiscoverSpec {
	return &simplyblockv1alpha2.DiscoverSpec{
		NetworkAttachment: &simplyblockv1alpha2.NetworkAttachmentSelection{Management: management, Data: data},
	}
}

// attachedHost is an ordinary worker with four disks, reporting the namespace's
// definitions as the options say.
func attachedHost(name string, opts ...hostOpt) nodeprobe.Report {
	return host(name, append([]hostOpt{disks(spread(3*tb, 4)...)}, opts...)...)
}

// attachedFleet is two ordinary workers in a cluster running Multus.
func attachedFleet(opts ...hostOpt) []nodeprobe.Report {
	served := definitions("Served", storageData, storageMgmt)
	return []nodeprobe.Report{
		attachedHost("worker-01", append([]hostOpt{served}, opts...)...),
		attachedHost("worker-02", served),
	}
}

func attachmentCases() map[string]Case {
	attached := func(slug string, reports []nodeprobe.Report, run *simplyblockv1alpha2.DiscoverSpec) Case {
		return Case{Family: "net", Slug: slug, Reports: reports, Nodes: kubeFleet(reports), Discover: run}
	}
	withoutMultus := []nodeprobe.Report{
		attachedHost("worker-01", definitions("NotServed")),
		attachedHost("worker-02", definitions("NotServed")),
	}
	return map[string]Case{
		"NET-71": attached("a-management-attachment-alone", attachedFleet(), attachedThrough("storage-mgmt", "")),
		"NET-72": attached("a-management-and-a-data-attachment", attachedFleet(),
			attachedThrough("storage-mgmt", "storage-data")),
		"NET-73": attached("an-attachment-on-a-cluster-without-multus", withoutMultus, attachedThrough("storage-mgmt", "")),
		"NET-74": attached("an-attachment-the-namespace-does-not-hold", attachedFleet(), attachedThrough("storage-nope", "")),
		"NET-75": attached("a-worker-without-the-attachments-host-nic", attachedFleet(withoutData()),
			attachedThrough("storage-mgmt", "storage-data")),
	}
}
