// Tests of which workers the management-network comparison keeps: the ones the
// cluster already reaches on the interface chosen, whatever subnet each is on,
// and the ones a ranking chose only where they share a network with the rest.
package discovery

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// managedOn is uniformFleet with every worker carrying one NIC that holds the
// address given for it, in the order given.
func managedOn(addresses ...string) []nodeprobe.Report {
	reports := uniformFleet(len(addresses))
	for i, address := range addresses {
		reports[i].Interfaces = []nodeprobe.Interface{
			iface(eth0, func(x *nodeprobe.Interface) { x.Addresses = []string{address} }),
		}
	}
	return reports
}

// reachedOn is the node objects of a fleet, each worker reached by the cluster
// on the address given for it. An empty address is a node that states none.
func reachedOn(reports []nodeprobe.Report, addresses ...string) map[string]KubeNode {
	nodes := make([]corev1.Node, 0, len(reports))
	for i, report := range reports {
		node := kubeWorker(report.Node)
		if addresses[i] != "" {
			node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: addresses[i]},
			}
		}
		nodes = append(nodes, node)
	}
	return KubeNodesOf(nodes)
}

// managementRefusals is the workers the management-network rule refused.
func managementRefusals(plan Plan) []string {
	var out []string
	for _, refusal := range plan.Refusals {
		if refusal.Rule == ManagementNetworkRule {
			out = append(out, refusal.Worker)
		}
	}
	return out
}

// Regression: 2026-11-11-discovery-multi-az-management-subnets — three workers
// in three availability zones, each on its zone's own subnet, were drafted as a
// one-worker cluster. No subnet was held by a majority, the tie went to the
// lowest network, and the other two workers were refused although the cluster
// reaches every one of them on the interface chosen.
func TestWorkersManagedOnTheirNodeAddressesAreKeptAcrossSubnets(t *testing.T) {
	reports := managedOn("10.248.192.17/23", "10.248.195.56/23", "10.248.196.19/23")
	planner := Planner{KubeNodes: reachedOn(reports, "10.248.192.17", "10.248.195.56", "10.248.196.19")}

	plan := planner.Plan(reports, nil)

	if refused := managementRefusals(plan); len(refused) != 0 {
		t.Errorf("the management network rule refused %v, want none: the cluster reaches each on its interface",
			refused)
	}
	if len(plan.Workers) != 3 {
		t.Fatalf("kept %d workers, want all three; refusals: %v", len(plan.Workers), plan.RefusalLines())
	}
}

// The minority is kept as well as a tie: one worker of four in another subnet is
// the fourth availability zone, not a machine that chose the wrong NIC.
func TestAWorkerManagedOnItsNodeAddressIsKeptOffTheMajoritySubnet(t *testing.T) {
	reports := managedOn("10.10.10.1/24", "10.10.10.2/24", "10.10.10.3/24", "10.10.20.4/24")
	planner := Planner{KubeNodes: reachedOn(reports, "10.10.10.1", "10.10.10.2", "10.10.10.3", "10.10.20.4")}

	plan := planner.Plan(reports, nil)

	if len(plan.Workers) != 4 {
		t.Fatalf("kept %d workers, want all four; refusals: %v", len(plan.Workers), plan.RefusalLines())
	}
}

// With no node address to go by, the interface is a ranking's choice and the
// comparison is what catches a wrong one: the majority rule holds as before.
func TestAWorkerRankedOntoAnotherNetworkIsRefused(t *testing.T) {
	reports := managedOn("10.10.10.1/24", "10.10.10.2/24", "10.10.10.3/24", "10.10.20.4/24")

	plan := Planner{}.Plan(reports, nil)

	refused := managementRefusals(plan)
	if len(refused) != 1 || refused[0] != "worker-4" {
		t.Fatalf("the management network rule refused %v, want only worker-4", refused)
	}
}

// A worker whose interface was ranked is held to the networks the cluster
// reaches the others on, and not to a count: the three subnets here are each
// held once, and the fourth worker is on none of them.
func TestARankedWorkerOffEveryNetworkTheClusterReachesIsRefused(t *testing.T) {
	reports := managedOn("10.248.192.17/23", "10.248.195.56/23", "10.248.196.19/23", "172.16.0.4/24")
	planner := Planner{KubeNodes: reachedOn(reports, "10.248.192.17", "10.248.195.56", "10.248.196.19", "")}

	plan := planner.Plan(reports, nil)

	refused := managementRefusals(plan)
	if len(refused) != 1 || refused[0] != "worker-4" {
		t.Fatalf("the management network rule refused %v, want only worker-4; refusals: %v",
			refused, plan.RefusalLines())
	}
	for _, refusal := range plan.Refusals {
		if refusal.Rule != ManagementNetworkRule {
			continue
		}
		for _, network := range []string{"172.16.0.0/24", "10.248.192.0/23", "10.248.194.0/23", "10.248.196.0/23"} {
			if !strings.Contains(refusal.Reason, network) {
				t.Errorf("the refusal reads %q, want it to name %s", refusal.Reason, network)
			}
		}
	}
}

// A ranked worker on one of the networks the cluster reaches the others on is
// kept, though that network is not a majority's.
func TestARankedWorkerOnANetworkTheClusterReachesIsKept(t *testing.T) {
	reports := managedOn("10.248.192.17/23", "10.248.195.56/23", "10.248.196.19/23", "10.248.196.20/23")
	planner := Planner{KubeNodes: reachedOn(reports, "10.248.192.17", "10.248.195.56", "10.248.196.19", "")}

	plan := planner.Plan(reports, nil)

	if len(plan.Workers) != 4 {
		t.Fatalf("kept %d workers, want all four; refusals: %v", len(plan.Workers), plan.RefusalLines())
	}
}
