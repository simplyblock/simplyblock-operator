// The network-interface cases, §7 of the document.
//
// The ladder that picks a management interface is five rungs, and each case
// here is a host where exactly one of them decides. The stacked kinds are the
// reason the section is the longest: a bond, a VLAN, a bridge, and a veth are
// all virtual devices, and which of them an address can be bound to differs for
// every one.
//
// Every worker carries the same four disks, because a worker with none is
// refused before its interfaces are read and the case would be about nothing.

package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/atlas/inventory"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// reachedAt is the address the cluster reaches worker-01 on, which is what
// makes an interface the management one outright.
const reachedAt = "10.10.10.1"

// netHost is a worker whose disks are ordinary and whose interfaces are the
// case.
func netHost(name string, list ...nodeprobe.Interface) nodeprobe.Report {
	return host(name, cpu(1, 16, 2), disks(spread(3*tb, 4)...), ifaces(list...))
}

// known is the case where Kubernetes knows the address it reaches each worker
// on, which is the ordinary run.
func known(reports ...nodeprobe.Report) Case {
	return Case{Family: "net", Reports: reports, Nodes: kubeFleet(reports)}
}

// unknown is the case where no node object names an address, so the ranking
// decides rather than the address.
func unknown(reports ...nodeprobe.Report) Case {
	nodes := make([]corev1.Node, 0, len(reports))
	for _, report := range reports {
		nodes = append(nodes, kubeNode(report.Node))
	}
	return Case{Family: "net", Reports: reports, Nodes: nodes}
}

// bonded is the interface list of a host whose management network is a bond
// over two NICs, with the address wherever the case puts it.
func bonded(addresses map[string][]string) []nodeprobe.Interface {
	holds := func(name string) ifaceOpt { return holding(addresses[name]...) }
	return []nodeprobe.Interface{
		nic("bond0", inventory.LinkBond, over("eth0", "eth1"), under("bond0.100"), holds("bond0")),
		nic("bond0.100", inventory.LinkVLAN, over("bond0"), holds("bond0.100")),
		nic("eth0", inventory.LinkPhysical, at(25000), on(0), under("bond0"),
			slotted("0000:3b:00.0"), holds("eth0")),
		nic("eth1", inventory.LinkPhysical, at(25000), on(0), under("bond0"),
			slotted("0000:3b:00.1"), holds("eth1")),
		nic("lo", inventory.LinkLoopback, holding("127.0.0.1")),
	}
}

func netCases() map[string]Case {
	// The cluster's own plumbing, which every worker of every Kubernetes fleet
	// carries and none of it is a management interface.
	//
	// It builds a list rather than being one, because the case that puts a NIC
	// beside it would otherwise append into the slice every other case shares.
	plumbing := func(beside ...nodeprobe.Interface) []nodeprobe.Interface {
		out := make([]nodeprobe.Interface, 0, 3+len(beside))
		out = append(out,
			nic("cni0", inventory.LinkBridge, holding("10.42.2.1")),
			nic("flannel.1", inventory.LinkVXLAN, holding("10.42.2.0")),
			nic("lo", inventory.LinkLoopback, holding("127.0.0.1")),
		)
		return append(out, beside...)
	}

	veths := func(n int) []nodeprobe.Interface {
		out := make([]nodeprobe.Interface, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, nic(fmt.Sprintf("veth%04x", 0x7a10+i), inventory.LinkVirtual,
				holding(fmt.Sprintf("10.42.2.%d", 10+i))))
		}
		return out
	}

	cases := map[string]Case{
		"NET-01": known(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), holding(reachedAt)))),

		"NET-02": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(1000), holding("192.168.10.1")),
			nic("eth1", inventory.LinkPhysical, at(10000), holding("10.10.11.1")))),

		"NET-03": known(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(1000), holding(reachedAt)),
			nic("eth1", inventory.LinkPhysical, at(10000), holding("10.10.11.1")))),

		"NET-04": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(1000), linkState("down"), holding("192.168.10.1")),
			nic("eth1", inventory.LinkPhysical, at(10000), holding("169.254.1.1")),
			nic("cni0", inventory.LinkBridge, at(1000), holding("10.42.2.1")),
			nic("br0", inventory.LinkBridge, at(10000), holding("192.168.1.1")))),

		"NET-05": unknown(host("worker-01", cpu(1, 16, 2), disks(spread(3*tb, 4)...), ifaces())),

		"NET-06": unknown(
			netHost("worker-01", nic("eth0", inventory.LinkPhysical, at(25000), holding("192.168.10.1"))),
			netHost("worker-02", nic("ens5f0", inventory.LinkPhysical, at(25000), holding("192.168.10.2")))),

		"NET-07": unknown(netHost("worker-01",
			nic("eth1", inventory.LinkPhysical, at(10000), holding("10.10.11.1")),
			nic("eth0", inventory.LinkPhysical, at(10000), holding("192.168.10.1")))),

		"NET-08": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, holding("192.168.10.1")),
			nic("eth1", inventory.LinkPhysical, at(10000), linkState("down"), holding("10.10.11.1")))),

		"NET-09": known(netHost("worker-01",
			nic("br0", inventory.LinkBridge, over("eth0"), holding(reachedAt)),
			nic("eth0", inventory.LinkPhysical, at(10000), under("br0"), slotted("0000:3b:00.0")))),

		"NET-10": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), holding("2001:db8::1")))),

		"NET-11": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), holding("169.254.1.1", "192.168.10.1")))),

		"NET-12": unknown(netHost("worker-01",
			nic("lo", inventory.LinkLoopback, holding("127.0.0.1")))),

		"NET-13": known(netHost("worker-01", bonded(map[string][]string{"bond0": {reachedAt}})...)),

		"NET-14": known(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(25000), under("eth0.100"), slotted("0000:3b:00.0")),
			nic("eth0.100", inventory.LinkVLAN, over("eth0"), holding(reachedAt)))),

		"NET-15": known(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), holding(reachedAt)),
			nic("vxlan.calico", inventory.LinkVXLAN, holding("10.42.2.0")))),

		"NET-16": known(netHost("worker-01", bonded(map[string][]string{"bond0.100": {reachedAt}})...)),

		"NET-17": known(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(25000), under("macvlan0", "ipvlan0"),
				holding(reachedAt)),
			nic("macvlan0", inventory.LinkMACVLAN, over("eth0"), holding("192.168.10.5")),
			nic("ipvlan0", inventory.LinkIPVLAN, over("eth0"), holding("192.168.10.6")))),

		"NET-18": known(netHost("worker-01", append(veths(12),
			nic("eth0", inventory.LinkPhysical, at(10000), holding(reachedAt)))...)),

		"NET-19": unknown(netHost("worker-01",
			nic("cni0", inventory.LinkBridge, over("veth7a10"), holding("10.42.2.1")),
			nic("docker0", inventory.LinkBridge, holding("172.17.0.1")),
			nic("eth0", inventory.LinkPhysical, at(10000), slotted("0000:3b:00.0")))),

		"NET-20": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), linkState("dormant"), holding("192.168.10.1")),
			nic("eth1", inventory.LinkPhysical, at(10000), linkState("lowerlayerdown"),
				holding("10.10.11.1")))),

		"NET-21": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), linkState("unknown"),
				holding("192.168.10.1")))),

		"NET-22": unknown(netHost("worker-01",
			nic("ens5f0", inventory.LinkPhysical, at(10000), holding("192.168.10.1")),
			nic("eth0", inventory.LinkPhysical, at(25000), holding("10.10.11.1")))),

		"NET-23": known(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), holding(reachedAt)),
			nic("ens5f1", inventory.LinkPhysical, at(100000), on(0), holding("192.168.20.1")),
			nic("ens5f2", inventory.LinkPhysical, at(100000), on(0), holding("192.168.21.1")))),

		"NET-24": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), holding("192.168.10.1")),
			nic("ens5f0", inventory.LinkPhysical, at(100000), holding("192.168.20.1")))),

		"NET-25": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), frames(9000), holding("192.168.10.1")),
			nic("eth1", inventory.LinkPhysical, at(10000), holding("10.10.11.1")))),

		"NET-26": unknown(netHost("worker-01",
			nic("br0", inventory.LinkBridge, at(100000), over("eth1"), holding("192.168.1.1")),
			nic("eth0", inventory.LinkPhysical, at(10000), holding("192.168.10.1")),
			nic("eth1", inventory.LinkPhysical, at(100000), under("br0"), slotted("0000:3b:00.0")))),

		"NET-27": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), holding("192.168.10.1")))),

		"NET-28": unknown(netHost("worker-01", append(
			bonded(map[string][]string{"bond0": {"192.168.10.1"}}),
			nic("eth2", inventory.LinkPhysical, at(10000), slotted("0000:af:00.0"),
				holding("192.168.20.1")))...)),

		"NET-29": unknown(netHost("worker-01", append(
			bonded(map[string][]string{"bond0.100": {"192.168.10.1"}}),
			nic("eth2", inventory.LinkPhysical, at(10000), slotted("0000:af:00.0"),
				holding("192.168.20.1")))...)),

		"NET-31": known(netHost("worker-01",
			nic("veth7a1c", inventory.LinkVirtual, holding(reachedAt)),
			nic("lo", inventory.LinkLoopback, holding("127.0.0.1")))),

		"NET-33": known(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), linkState("down"), holding(reachedAt)))),

		"NET-34": known(netHost("worker-01",
			bonded(map[string][]string{"bond0": {reachedAt}, "bond0.100": {reachedAt}})...)),

		"NET-35": unknown(netHost("worker-01", append(
			bonded(map[string][]string{"bond0": {"192.168.10.1"}})[:1],
			nic("eth0", inventory.LinkPhysical, at(25000), on(0), under("bond0"),
				slotted("0000:3b:00.0")),
			nic("eth1", inventory.LinkPhysical, at(25000), on(0), under("bond0"),
				slotted("0000:3b:00.1")))...)),

		"NET-36": known(netHost("worker-01",
			nic("bond0", inventory.LinkBond, over("eth9"), holding(reachedAt)))),

		"NET-37": known(netHost("worker-01",
			nic("team0", inventory.LinkTeam, over("eth0", "eth1"), holding(reachedAt)),
			nic("eth0", inventory.LinkPhysical, at(25000), on(0), under("team0"),
				slotted("0000:3b:00.0")),
			nic("eth1", inventory.LinkPhysical, at(25000), on(0), under("team0"),
				slotted("0000:3b:00.1")))),

		"NET-38": known(netHost("worker-01",
			nic("br0", inventory.LinkBridge, over("bond0"), holding(reachedAt)),
			nic("bond0", inventory.LinkBond, over("eth0", "eth1"), under("br0")),
			nic("eth0", inventory.LinkPhysical, at(25000), on(0), under("bond0"),
				slotted("0000:3b:00.0")),
			nic("eth1", inventory.LinkPhysical, at(25000), on(0), under("bond0"),
				slotted("0000:3b:00.1")))),

		"NET-39": known(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(25000), on(0), under("macvlan0"),
				slotted("0000:3b:00.0")),
			nic("macvlan0", inventory.LinkMACVLAN, over("eth0"), holding(reachedAt)))),

		"NET-40": unknown(netHost("worker-01",
			nic("eth0", inventory.LinkPhysical, at(10000), unkinded(), holding("192.168.10.1")),
			nic("cni0", inventory.LinkBridge, unkinded(), holding("10.42.2.1")),
			nic("flannel.1", inventory.LinkVXLAN, unkinded(), holding("10.42.2.0")))),

		"NET-41": known(netHost("worker-01", bonded(map[string][]string{"bond0": {reachedAt}})...)),

		"NET-42": known(netHost("worker-01", bonded(map[string][]string{"bond0": {reachedAt}})...)),
	}

	// A bond whose members sit in two sockets, which has no memory node at all.
	split := bonded(map[string][]string{"bond0": {reachedAt}})
	for i := range split {
		if split[i].Name == "eth1" {
			split[i].NUMANode = 1
		}
	}
	cases["NET-30"] = known(netHost("worker-01", split...))

	// The cluster's own plumbing beside nothing else, which is the worker a
	// draft has to leave without an interface.
	cases["NET-19"] = unknown(netHost("worker-01", plumbing(
		nic("eth0", inventory.LinkPhysical, at(10000), slotted("0000:3b:00.0")))...))

	// The seam case: a stack that points at itself cannot be written by the
	// kernel, so it is built directly in the discovery package.
	cases["NET-32"] = Case{
		Family: "net", Slug: "a-stack-that-points-at-itself",
		Note: "Driven in the discovery package, because no probe can write a report whose " +
			"lower_* links form a cycle and the case is about the resolution terminating anyway.",
	}

	slugs := map[string]string{
		"NET-01": "one-addressed-physical-nic",
		"NET-02": "the-faster-of-two-nics",
		"NET-03": "the-node-address-beats-the-faster-link",
		"NET-04": "every-interface-unusable",
		"NET-05": "no-interfaces-reported",
		"NET-06": "two-workers-naming-their-nics-differently",
		"NET-07": "two-equal-nics",
		"NET-08": "an-addressed-nic-reporting-no-speed",
		"NET-09": "the-node-address-on-a-host-bridge",
		"NET-10": "a-global-ipv6-address-only",
		"NET-11": "a-link-local-and-a-routable-address",
		"NET-12": "loopback-and-nothing-else",
		"NET-13": "the-node-address-on-a-bond",
		"NET-14": "the-node-address-on-a-vlan",
		"NET-15": "an-overlay-beside-an-addressed-nic",
		"NET-16": "the-node-address-on-a-vlan-over-a-bond",
		"NET-17": "a-macvlan-and-an-ipvlan-over-one-nic",
		"NET-18": "twelve-veths-beside-one-nic",
		"NET-19": "the-clusters-own-plumbing-alone",
		"NET-20": "a-dormant-link-and-a-down-lower-layer",
		"NET-21": "a-link-whose-state-is-unknown",
		"NET-22": "a-fleet-that-knows-which-nic-it-means",
		"NET-23": "a-fleet-that-knows-its-data-nics",
		"NET-24": "a-fast-link-and-a-slow-one",
		"NET-25": "jumbo-frames-against-standard-ones",
		"NET-26": "a-fast-bridge-against-a-slower-nic",
		"NET-27": "a-link-up-with-no-partner",
		"NET-28": "an-aggregate-with-no-speed-of-its-own",
		"NET-29": "a-vlan-inheriting-the-bonds-speed",
		"NET-30": "a-bond-across-two-sockets",
		"NET-31": "a-veth-holding-the-node-address",
		"NET-33": "the-node-address-on-a-down-link",
		"NET-34": "the-node-address-on-two-interfaces",
		"NET-35": "an-aggregate-reporting-its-own-speed",
		"NET-36": "a-member-the-report-does-not-carry",
		"NET-37": "a-team-interface",
		"NET-38": "a-bridge-over-a-bond",
		"NET-39": "a-macvlan-holding-the-node-address",
		"NET-40": "an-interface-naming-no-kind",
		"NET-41": "a-bonded-data-path-ranked-for-placement",
		"NET-42": "what-the-draft-says-about-a-bonded-host",
	}
	gaps := map[string]string{
		"NET-04": "G-7", "NET-22": "G-23", "NET-23": "G-24", "NET-27": "G-26",
		"NET-41": "G-27", "NET-42": "G-28", "NET-06": "G-25",
	}
	for id, slug := range slugs {
		entry := cases[id]
		entry.Slug = slug
		entry.Gap = gaps[id]
		cases[id] = entry
	}
	return cases
}
