// Tests that a draft states the data NICs it chose as spec.cluster.network host
// interfaces, keeps the management NIC in the group's mgmtInterface, and puts a
// fleet with no data NIC on the pod network.
package discovery

import (
	"fmt"
	"testing"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// storageNICFleet is uniformFleet with every worker carrying a management NIC
// and a faster storage NIC, each holding an address, so that the draft has a
// data interface to name.
func storageNICFleet(n int) []nodeprobe.Report {
	reports := uniformFleet(n)
	for i := range reports {
		reports[i].Interfaces = []nodeprobe.Interface{
			iface(eth0, func(x *nodeprobe.Interface) { x.Addresses = []string{fmt.Sprintf("192.168.10.%d/24", 10+i)} }),
			iface(eth1, func(x *nodeprobe.Interface) {
				x.Addresses = []string{fmt.Sprintf("10.10.10.%d/24", 10+i)}
				x.SpeedMbps, x.MTU, x.Kind = 40000, 9000, "physical"
			}),
		}
	}
	return reports
}

func TestTheDraftStatesTheInterfacesAsAHostInterfaceNetwork(t *testing.T) {
	plan := Planner{}.Plan(storageNICFleet(3), nil)

	group := onlyGroup(t, plan)
	if group.MgmtInterface != eth0 {
		t.Errorf("the group's mgmtInterface is %q, want the chosen %s", group.MgmtInterface, eth0)
	}
	if len(group.DataInterfaces) != 1 || group.DataInterfaces[0] != eth1 {
		t.Errorf("the group names the data interfaces %v, want [%s]", group.DataInterfaces, eth1)
	}

	template, err := ClusterTemplateFor("c", plan, TemplateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	network := template.Template.Network
	if network == nil || !network.SpdkOnHostNetwork {
		t.Fatalf("the template's network is %+v, want the host network", network)
	}
}

// Each node names its own NICs, so a worker that calls them something else stays
// in the draft with a group of its own.
func TestAWorkerWithOtherInterfaceNamesKeepsItsOwnGroup(t *testing.T) {
	reports := storageNICFleet(3)
	reports[2].Interfaces[0].Name = "ens5f0"
	reports[2].Interfaces[1].Name = "ens6f0"

	plan := Planner{}.Plan(reports, nil)

	if len(plan.Workers) != 3 {
		t.Fatalf("kept %d workers, want all three; refusals: %v", len(plan.Workers), plan.RefusalLines())
	}
	names := map[string]string{}
	for _, set := range plan.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				names[worker] = group.MgmtInterface + "/" + group.DataInterfaces[0]
			}
		}
	}
	if names["worker-1"] != eth0+"/"+eth1 || names["worker-3"] != "ens5f0/ens6f0" {
		t.Errorf("the groups name %v, want eth0/eth1 for worker-1 and ens5f0/ens6f0 for worker-3", names)
	}
}

func TestAFleetWithNoDataInterfaceIsDraftedOnThePodNetwork(t *testing.T) {
	plan := Planner{}.Plan(uniformFleet(3), nil)

	template, err := ClusterTemplateFor("c", plan, TemplateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	network := template.Template.Network
	if network == nil || !network.PodNetwork {
		t.Fatalf("the template's network is %+v, want the pod network", network)
	}
}
