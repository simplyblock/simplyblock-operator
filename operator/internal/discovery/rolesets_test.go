// What the draft looks like when a fleet has more than one kind of machine.
//
// The property that matters is the one a reviewer acts on: the infrastructure
// tier is a block of its own, and it comes first. A reviewer who wants only those
// disks deletes the block below it, rather than moving hostnames between them.

package discovery

import (
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// machine is a worker the planner has already chosen devices for. The devices
// themselves do not matter here: what these cases are about is which set a
// machine lands in, which its role decides and its disks do not.
func machine(name string, role NodeRole) Worker {
	return Worker{Name: name, Kube: KubeNode{Name: name, Role: role}}
}

// grouped is one hardware group holding the given machines.
func grouped(name string, workers ...Worker) Group {
	return Group{Name: name, Workers: workers, Class: ClassNVMe}
}

// A fleet with both tiers gets two blocks, and the infrastructure one is first.
func TestTheInfrastructureSetComesFirst(t *testing.T) {
	sets := SplitByRole{}.Build([]Group{grouped("uniform",
		machine("worker-1", NodeRoleWorker),
		machine("infra-1", NodeRoleInfra),
		machine("worker-2", NodeRoleWorker),
	)})

	if len(sets) != 2 {
		t.Fatalf("built %d node sets, want one per role: %+v", len(sets), sets)
	}
	if sets[0].Name != "infra" {
		t.Errorf("the first set is %q, want the infrastructure tier", sets[0].Name)
	}
	if sets[1].Name != DefaultNodeSetName {
		t.Errorf("the second set is %q, want the workers", sets[1].Name)
	}

	if got := workersOf(sets[0]); len(got) != 1 || got[0] != "infra-1" {
		t.Errorf("the infrastructure set holds %v", got)
	}
	if got := workersOf(sets[1]); len(got) != 2 {
		t.Errorf("the worker set holds %v, want both workers", got)
	}
}

// A fleet with no infrastructure tier gets exactly what it got before: one set,
// named for what it is.
func TestAFleetOfPlainWorkersGetsOneSet(t *testing.T) {
	sets := SplitByRole{}.Build([]Group{grouped("uniform",
		machine("worker-1", NodeRoleWorker),
		machine("worker-2", NodeRoleWorker),
	)})

	if len(sets) != 1 {
		t.Fatalf("built %d node sets, want one: %+v", len(sets), sets)
	}
	if sets[0].Name != DefaultNodeSetName {
		t.Errorf("the set is %q, want %q", sets[0].Name, DefaultNodeSetName)
	}
}

// The hardware grouping survives the split: two infrastructure nodes with the
// same disks stay one group, which is what keeps a document short.
func TestTheHardwareGroupingSurvivesTheSplit(t *testing.T) {
	sets := SplitByRole{}.Build([]Group{
		grouped("dense", machine("infra-1", NodeRoleInfra), machine("infra-2", NodeRoleInfra)),
		grouped("sparse", machine("worker-1", NodeRoleWorker)),
	})

	if len(sets) != 2 {
		t.Fatalf("built %d node sets: %+v", len(sets), sets)
	}
	infra := sets[0]
	if len(infra.Groups) != 1 {
		t.Fatalf("the infrastructure set has %d groups, want one for identical hardware",
			len(infra.Groups))
	}
	if len(infra.Groups[0].Workers) != 2 {
		t.Errorf("the group holds %v, want both machines", infra.Groups[0].Workers)
	}
}

// One hardware group holding machines of two roles is split between the sets
// rather than landing in whichever one sorted first.
func TestAMixedGroupIsSplitBetweenTheSets(t *testing.T) {
	sets := SplitByRole{}.Build([]Group{grouped("identical",
		machine("infra-1", NodeRoleInfra),
		machine("worker-1", NodeRoleWorker),
	)})

	if len(sets) != 2 {
		t.Fatalf("built %d node sets: %+v", len(sets), sets)
	}
	for _, set := range sets {
		if got := workersOf(set); len(got) != 1 {
			t.Errorf("set %q holds %v, want the one machine of its role", set.Name, got)
		}
	}
}

// A machine the planner was given no node object for is a worker, because a
// machine with no role label is one.
func TestAMachineWithNoKubeNodeIsAWorker(t *testing.T) {
	sets := SplitByRole{}.Build([]Group{{
		Name:    "unknown",
		Workers: []Worker{{Name: "worker-1"}},
		Class:   ClassNVMe,
	}})

	if len(sets) != 1 || sets[0].Name != DefaultNodeSetName {
		t.Fatalf("built %+v, want the one worker set", sets)
	}
}

// workersOf flattens a set's groups to the hostnames in it.
func workersOf(set simplyblockv1alpha2.NodeSet) []string {
	var out []string
	for _, group := range set.Groups {
		out = append(out, group.Workers...)
	}
	return out
}
