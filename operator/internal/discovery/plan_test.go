// The pipeline end to end: reports in, node sets out, and everything declined
// accounted for.
//
// Two properties get the most attention here. The first is that grouping is by
// what the workers actually have, because a NodeGroup's device selection is
// shared by its workers and a group spanning two hardware layouts claims each
// worker has the other's disks. The second is determinism: a discovery run is
// re-run against a growing fleet, and two runs that produced different drafts
// from the same hardware would make the second one unreviewable.

package discovery

import (
	"slices"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

func TestPlanPutsAUniformFleetInOneGroupOfOneNodeSet(t *testing.T) {
	plan := Planner{}.Plan(uniformFleet(4), nil)

	if len(plan.NodeSets) != 1 {
		t.Fatalf("built %d node sets, want 1: nothing a probe reports says which rack "+
			"a worker is in", len(plan.NodeSets))
	}
	set := plan.NodeSets[0]
	if set.Name != DefaultNodeSetName {
		t.Errorf("the node set is called %q, want %q", set.Name, DefaultNodeSetName)
	}
	if len(set.Groups) != 1 {
		t.Fatalf("built %d groups from an identical fleet, want 1: %+v", len(set.Groups), set.Groups)
	}

	group := set.Groups[0]
	if !slices.Equal(group.Workers, []string{"worker-1", "worker-2", "worker-3", "worker-4"}) {
		t.Errorf("the group holds %v", group.Workers)
	}
	if group.Devices == nil || len(group.Devices.NVMe) != 2 {
		t.Fatalf("the group's devices are %+v, want the two on the chosen memory node", group.Devices)
	}
	if len(group.Devices.Block) != 0 {
		t.Error("an NVMe run named block devices")
	}
}

func TestPlanSplitsWorkersWhoseHardwareDiffers(t *testing.T) {
	// One worker has a disk in a different slot, and it is a slot the placement
	// actually chooses: grouping is by the devices a group hands over, so a
	// difference among the disks nobody takes is not a difference. A single
	// group here would tell the expansion that worker-4 has a disk at a slot it
	// does not.
	fleet := uniformFleet(4)
	fleet[3].Devices[0].PCIAddress = "0000:c0:00.0"

	plan := Planner{}.Plan(fleet, nil)

	if len(plan.NodeSets) != 1 {
		t.Fatalf("built %d node sets, want 1", len(plan.NodeSets))
	}
	groups := plan.NodeSets[0].Groups
	if len(groups) != 2 {
		t.Fatalf("built %d groups, want 2: one worker's slots differ: %+v", len(groups), groups)
	}

	byWorker := map[string][]string{}
	for _, group := range groups {
		for _, worker := range group.Workers {
			byWorker[worker] = group.Devices.NVMe
		}
	}
	if slices.Equal(byWorker["worker-1"], byWorker["worker-4"]) {
		t.Errorf("worker-1 and worker-4 were given the same devices %v despite differing slots",
			byWorker["worker-1"])
	}
	if !slices.Contains(byWorker["worker-4"], "0000:c0:00.0") {
		t.Errorf("worker-4's group names %v, and its own slot is missing", byWorker["worker-4"])
	}
}

func TestPlanIsTheSameWhateverOrderTheReportsArriveIn(t *testing.T) {
	// The reports arrive as the probe Jobs finish, which is not an order.
	fleet := uniformFleet(5)
	reversed := slices.Clone(fleet)
	slices.Reverse(reversed)

	first := Planner{}.Plan(fleet, nil)
	second := Planner{}.Plan(reversed, nil)

	if len(first.NodeSets) != len(second.NodeSets) {
		t.Fatalf("built %d and %d node sets", len(first.NodeSets), len(second.NodeSets))
	}
	for i := range first.NodeSets {
		a, b := first.NodeSets[i], second.NodeSets[i]
		if a.Name != b.Name || len(a.Groups) != len(b.Groups) {
			t.Fatalf("node set %d differs: %+v against %+v", i, a, b)
		}
		for j := range a.Groups {
			if a.Groups[j].Name != b.Groups[j].Name {
				t.Errorf("group %d is called %q and %q", j, a.Groups[j].Name, b.Groups[j].Name)
			}
			if !slices.Equal(a.Groups[j].Workers, b.Groups[j].Workers) {
				t.Errorf("group %d holds %v and %v", j, a.Groups[j].Workers, b.Groups[j].Workers)
			}
			if !slices.Equal(a.Groups[j].Devices.NVMe, b.Groups[j].Devices.NVMe) {
				t.Errorf("group %d names %v and %v", j, a.Groups[j].Devices.NVMe, b.Groups[j].Devices.NVMe)
			}
		}
	}
}

func TestPlanRecordsWhatItDeclinedAndWhy(t *testing.T) {
	// The boot disk, a partition of it, and a disk an LVM group holds. None
	// reaches the draft, and each is accounted for: an administrator whose disk
	// is missing has to be able to find out which rule took it.
	boot := refused(disk("nvme4n1", "0000:d0:00.0", 0, tb), blockdev.ReasonMounted)
	part := disk("nvme4n1p1", "0000:d0:00.0", 0, tb)
	part.Kind = string(blockdev.KindPartition)
	held := refused(disk("nvme5n1", "0000:d1:00.0", 0, tb), blockdev.ReasonStacked)

	fleet := uniformFleet(1)
	fleet[0].Devices = append(fleet[0].Devices, boot, part, held)

	plan := Planner{}.Plan(fleet, nil)

	byDevice := map[string]Refusal{}
	for _, refusal := range plan.Refusals {
		byDevice[refusal.Device] = refusal
	}

	for device, wantRule := range map[string]string{
		"nvme4n1":   "available",
		"nvme4n1p1": "whole disk",
		"nvme5n1":   "available",
	} {
		refusal, recorded := byDevice[device]
		if !recorded {
			t.Errorf("%s is missing from the draft and from the refusals", device)
			continue
		}
		if refusal.Rule != wantRule {
			t.Errorf("%s was declined by %q, want %q", device, refusal.Rule, wantRule)
		}
		if refusal.Reason == "" {
			t.Errorf("%s was declined with no reason", device)
		}
		if refusal.Worker != "worker-1" {
			t.Errorf("%s was attributed to %q", device, refusal.Worker)
		}
	}

	// The disks the placement did not take are recorded too, with the placement
	// as the reason rather than a rule. Node 0 is chosen here, so these are
	// node 1's, and they are as absent from the draft as a refused disk is.
	for _, left := range []string{"nvme2n1", "nvme3n1"} {
		refusal, recorded := byDevice[left]
		if !recorded {
			t.Errorf("%s was left on the other memory node and is unaccounted for", left)
			continue
		}
		if !strings.Contains(refusal.Rule, "NUMA") {
			t.Errorf("%s was recorded against %q, want the placement", left, refusal.Rule)
		}
	}
}

func TestPlanRefusesAWorkerWithNothingUsableAndSaysSo(t *testing.T) {
	// A worker whose every disk is taken. It must not appear in the draft, and
	// it must appear in the refusals: silently dropping it would leave a
	// four-worker fleet producing a three-worker draft with no explanation.
	fleet := uniformFleet(2)
	for i := range fleet[1].Devices {
		fleet[1].Devices[i] = refused(fleet[1].Devices[i], blockdev.ReasonMounted)
	}

	plan := Planner{}.Plan(fleet, nil)

	if len(plan.Workers) != 1 || plan.Workers[0].Name != "worker-1" {
		t.Fatalf("the draft holds %d workers: %+v", len(plan.Workers), plan.Workers)
	}
	wholeWorker := false
	for _, refusal := range plan.Refusals {
		if refusal.Worker == "worker-2" && refusal.Device == "" {
			wholeWorker = true
			if refusal.Rule != "has devices" {
				t.Errorf("worker-2 was declined by %q", refusal.Rule)
			}
		}
	}
	if !wholeWorker {
		t.Error("worker-2 is absent from the draft and no refusal says the worker itself was declined")
	}
}

func TestPlanAppliesTheFiltersItWasGiven(t *testing.T) {
	fleet := uniformFleet(2)

	// A deny list on the slot every worker boots from, and a size range that
	// admits the rest.
	filter := &simplyblockv1alpha2.DeviceFilter{
		PcieDenyList:   []string{"0000:af:00.0"},
		DriveSizeRange: "1T-4T",
	}

	plan := Planner{}.Plan(fleet, filter)

	for _, worker := range plan.Workers {
		if slices.Contains(worker.Addresses(), "0000:af:00.0") {
			t.Errorf("%s kept the denied slot: %v", worker.Name, worker.Addresses())
		}
	}
	denied := false
	for _, refusal := range plan.Refusals {
		if strings.Contains(refusal.Reason, "deny list") {
			denied = true
		}
	}
	if !denied {
		t.Error("the deny list took a disk and no refusal says so")
	}
}

func TestPlanScansTheBlockClassWhenAskedTo(t *testing.T) {
	// The block class names devices by path, and a virtio disk has no PCI
	// address at all, so the same fleet yields a block draft or nothing.
	fleet := []nodeprobe.Report{report("worker-1",
		blockDisk("vdb", 0, 2*tb),
		blockDisk("vdc", 0, 2*tb),
	)}

	filter := &simplyblockv1alpha2.DeviceFilter{EnableLogicalBlockDevices: ptr.To(true)}
	plan := Planner{Class: ClassOf(filter)}.Plan(fleet, filter)

	if plan.Class != ClassBlock {
		t.Fatalf("the plan is for class %q, want %q", plan.Class, ClassBlock)
	}
	if len(plan.NodeSets) != 1 || len(plan.NodeSets[0].Groups) != 1 {
		t.Fatalf("built %+v", plan.NodeSets)
	}
	group := plan.NodeSets[0].Groups[0]
	if !slices.Equal(group.Devices.Block, []string{"/dev/vdb", "/dev/vdc"}) {
		t.Errorf("the group names %v, want the two paths", group.Devices.Block)
	}
	if len(group.Devices.NVMe) != 0 {
		t.Error("a block run named NVMe addresses")
	}

	// The same fleet under an NVMe run yields nothing, with a reason.
	nvme := Planner{}.Plan(fleet, nil)
	if len(nvme.NodeSets) != 0 {
		t.Errorf("an NVMe run built %+v from virtio disks", nvme.NodeSets)
	}
	if len(nvme.Refusals) == 0 {
		t.Error("an NVMe run declined every disk and recorded nothing")
	}
}

func TestPlanNamesADeviceOnceWhenAControllerHasTwoNamespaces(t *testing.T) {
	// Two namespaces of one controller report the same PCI address. The draft
	// names the controller, so a repeat is not a second device.
	fleet := []nodeprobe.Report{report("worker-1",
		disk("nvme0n1", "0000:5e:00.0", 0, tb),
		disk("nvme0n2", "0000:5e:00.0", 0, tb),
		disk("nvme1n1", "0000:5f:00.0", 0, tb),
	)}

	plan := Planner{}.Plan(fleet, nil)

	if len(plan.Workers) != 1 {
		t.Fatalf("the draft holds %d workers", len(plan.Workers))
	}
	if got := plan.Workers[0].Addresses(); !slices.Equal(got, []string{"0000:5e:00.0", "0000:5f:00.0"}) {
		t.Errorf("the worker names %v, want each controller once", got)
	}
}

func TestPlanSummaryCountsWhatItBuilt(t *testing.T) {
	plan := Planner{}.Plan(uniformFleet(3), nil)

	summary := plan.Summary()
	for _, fragment := range []string{"3 workers", "nvme", "1 group", "1 node set"} {
		if !strings.Contains(summary, fragment) {
			t.Errorf("the summary %q does not mention %q", summary, fragment)
		}
	}
}

func TestPlanBuildsNothingFromNothing(t *testing.T) {
	// A run against a fleet with no free storage produces no node sets rather
	// than a node set with no groups, which the API would refuse anyway.
	plan := Planner{}.Plan(nil, nil)

	if len(plan.NodeSets) != 0 || len(plan.Workers) != 0 {
		t.Errorf("built %+v from no reports", plan.NodeSets)
	}
	if plan.Summary() == "" {
		t.Error("an empty plan has nothing to say for itself")
	}
}

func TestPlanHonorsTheSeamsItWasGiven(t *testing.T) {
	// The point of the five seams is that one can be replaced without touching
	// the others, so a planner with a different placement and node set name
	// produces a different draft from the same reports.
	plan := Planner{
		Placement:      AllDevices{},
		NodeSetBuilder: SingleNodeSet{SetName: "rack-b"},
	}.Plan(uniformFleet(2), nil)

	if len(plan.NodeSets) != 1 || plan.NodeSets[0].Name != "rack-b" {
		t.Fatalf("built %+v, want one set called rack-b", plan.NodeSets)
	}
	group := plan.NodeSets[0].Groups[0]
	if len(group.Devices.NVMe) != 4 {
		t.Errorf("the group names %v, want all four disks: AllDevices ignores the memory node",
			group.Devices.NVMe)
	}
}
