// Tests for the severity and node a finding carries, and for the
// storage-network MTU check, the first check a run fails on.

package discovery

import (
	"testing"

	"github.com/simplyblock/atlas/inventory"

	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// onNetwork is a worker whose storage traffic runs over the named interfaces,
// each at the MTU given.
func onNetwork(name string, mtu int, data ...string) Worker {
	worker := Worker{Name: name, Report: nodeprobe.Report{Node: name}, Data: data}
	worker.Mgmt.Name = "eth0"
	worker.Report.Interfaces = append(worker.Report.Interfaces, nodeprobe.Interface{Name: "eth0", MTU: 1500})
	for _, iface := range data {
		worker.Report.Interfaces = append(worker.Report.Interfaces, nodeprobe.Interface{Name: iface, MTU: mtu})
	}
	return worker
}

func TestAMultipathFindingNamesItsWorkerAndWarns(t *testing.T) {
	plan := Plan{Workers: []Worker{withMultipath("worker-01", inventory.MultipathNotBuilt)}}
	plan.noteMultipath()

	if len(plan.Findings) != 1 {
		t.Fatalf("the findings are %+v, want one", plan.Findings)
	}
	found := plan.Findings[0]
	if found.Node != "worker-01" {
		t.Errorf("the finding names node %q, want worker-01", found.Node)
	}
	if found.Severity != SeverityWarn {
		t.Errorf("the finding is %q, want Warn: the worker still hosts a storage node", found.Severity)
	}
}

func TestAWorkerWhoseStorageMTUDiffersFromTheFleetFails(t *testing.T) {
	plan := Plan{Workers: []Worker{
		onNetwork("worker-01", 9000, "ens1f0"),
		onNetwork("worker-02", 9000, "ens1f0"),
		onNetwork("worker-03", 1500, "ens1f0"),
	}}
	plan.noteStorageMTU()

	if len(plan.Findings) != 1 {
		t.Fatalf("the findings are %+v, want one for worker-03", plan.Findings)
	}
	found := plan.Findings[0]
	if found.Reason != StorageMTUMismatch || found.Node != "worker-03" || found.Severity != SeverityFail {
		t.Errorf("the finding is %+v, want a Fail StorageMTUMismatch on worker-03", found)
	}
	if found.Remedy == "" {
		t.Error("a Fail finding carries no remedy")
	}
}

func TestTheManagementInterfaceIsTheStorageNetworkWithoutDataInterfaces(t *testing.T) {
	a, b := onNetwork("worker-01", 0), onNetwork("worker-02", 0)
	b.Report.Interfaces[0].MTU = 9000
	plan := Plan{Workers: []Worker{a, b}}
	plan.noteStorageMTU()

	if len(plan.Findings) != 1 {
		t.Fatalf("the findings are %+v, want one: the management interfaces disagree", plan.Findings)
	}
}

func TestAnAgreeingFleetOrAnUnreadMTUHasNoFinding(t *testing.T) {
	unread := onNetwork("worker-03", 0, "ens1f0")
	plan := Plan{Workers: []Worker{
		onNetwork("worker-01", 9000, "ens1f0"),
		onNetwork("worker-02", 9000, "ens1f0"),
		unread,
	}}
	plan.noteStorageMTU()

	if len(plan.Findings) != 0 {
		t.Errorf("the findings are %+v, want none: an unread MTU is not evidence of a mismatch", plan.Findings)
	}
}
