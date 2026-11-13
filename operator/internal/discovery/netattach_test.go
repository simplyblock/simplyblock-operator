// What a run that names NetworkAttachmentDefinitions drafts: the attachments in
// place of interfaces, host networking where the cluster has no Multus, and a
// worker left out with its reason where a definition cannot attach it.

package discovery

import (
	"reflect"
	"strings"
	"testing"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// The two definitions the attached fleet's namespace holds: a macvlan on the
// management NIC and an ipvlan on the data NIC, eth0 and eth1 on every worker
// report() builds.
var (
	mgmtDefinition = nodeprobe.NetworkAttachment{
		Namespace: "simplyblock", Name: "storage-mgmt", Type: "macvlan", HostInterface: "eth0", Mode: "bridge", IPAM: "whereabouts",
	}
	dataDefinition = nodeprobe.NetworkAttachment{
		Namespace: "simplyblock", Name: "storage-data", Type: "ipvlan", HostInterface: "eth1", Mode: "l2", IPAM: "static",
	}
)

// attachedFleet is uniformFleet with every worker reporting the namespace's
// definitions.
func attachedFleet(n int, definitions ...nodeprobe.NetworkAttachment) []nodeprobe.Report {
	reports := uniformFleet(n)
	for i := range reports {
		reports[i].NetworkAttachments = &nodeprobe.NetworkAttachments{
			State: "Served", Namespace: "simplyblock", Definitions: definitions,
		}
	}
	return reports
}

func attachedRun(management, data string) *simplyblockv1alpha2.DiscoverSpec {
	return &simplyblockv1alpha2.DiscoverSpec{
		NetworkAttachment: &simplyblockv1alpha2.NetworkAttachmentSelection{Management: management, Data: data},
	}
}

// onlyGroup is the single group a uniform fleet is drafted as.
func onlyGroup(t *testing.T, plan Plan) simplyblockv1alpha2.NodeGroup {
	t.Helper()
	if len(plan.NodeSets) != 1 || len(plan.NodeSets[0].Groups) != 1 {
		t.Fatalf("drafted %+v, want one group; refusals: %v", plan.NodeSets, plan.RefusalLines())
	}
	return plan.NodeSets[0].Groups[0]
}

func TestPlanNamesBothAttachmentsInPlaceOfInterfaces(t *testing.T) {
	plan := Planner{}.Plan(attachedFleet(3, dataDefinition, mgmtDefinition), attachedRun("storage-mgmt", "storage-data"))

	group := onlyGroup(t, plan)
	if len(group.Workers) != 3 {
		t.Errorf("the group holds %v, want every worker", group.Workers)
	}
	want := simplyblockv1alpha2.NetworkAttachmentSelection{Management: "storage-mgmt", Data: "storage-data"}
	if group.NetworkAttachment == nil || *group.NetworkAttachment != want {
		t.Errorf("the group's attachment is %+v, want %+v", group.NetworkAttachment, want)
	}
	if group.MgmtInterface != "" || len(group.DataInterfaces) > 0 {
		t.Errorf("an attached group names the interfaces %q and %v as well", group.MgmtInterface, group.DataInterfaces)
	}
}

// Data flows through the management attachment when it is the only one named,
// and the draft says so by naming no data attachment, as a host-networked group
// names no data interfaces.
func TestPlanWithOnlyAManagementAttachmentNamesNoDataAttachment(t *testing.T) {
	plan := Planner{}.Plan(attachedFleet(3, mgmtDefinition), attachedRun("storage-mgmt", ""))

	group := onlyGroup(t, plan)
	want := simplyblockv1alpha2.NetworkAttachmentSelection{Management: "storage-mgmt"}
	if group.NetworkAttachment == nil || *group.NetworkAttachment != want {
		t.Errorf("the group's attachment is %+v, want %+v", group.NetworkAttachment, want)
	}
}

// A run that names no attachment drafts host networking, whatever the cluster
// offers.
func TestPlanWithoutAnAttachmentKeepsHostNetworking(t *testing.T) {
	plan := Planner{}.Plan(attachedFleet(3, mgmtDefinition), nil)

	group := onlyGroup(t, plan)
	if group.NetworkAttachment != nil {
		t.Errorf("a run naming no attachment drafted %+v", *group.NetworkAttachment)
	}
	if want := onlyGroup(t, Planner{}.Plan(uniformFleet(3), nil)); !reflect.DeepEqual(group, want) {
		t.Errorf("drafted %+v, want what a fleet without definitions drafts: %+v", group, want)
	}
}

// Without Multus there is nothing to attach through, and the draft keeps host
// networking rather than proposing nothing.
func TestPlanKeepsHostNetworkingWhereTheKindIsNotServed(t *testing.T) {
	reports := uniformFleet(3)
	for i := range reports {
		reports[i].NetworkAttachments = &nodeprobe.NetworkAttachments{State: "NotServed", Namespace: "simplyblock"}
	}

	plan := Planner{}.Plan(reports, attachedRun("storage-mgmt", "storage-data"))

	group := onlyGroup(t, plan)
	if group.NetworkAttachment != nil {
		t.Errorf("a cluster without the kind was drafted with the attachment %+v", *group.NetworkAttachment)
	}
	if want := onlyGroup(t, Planner{}.Plan(uniformFleet(3), nil)); !reflect.DeepEqual(group, want) {
		t.Errorf("the fallback drafted %+v, want what a host-networked run drafts: %+v", group, want)
	}
	if !hasFinding(plan, NetworkAttachmentsUnavailable) {
		t.Errorf("the fallback is not reported: %+v", plan.Findings)
	}
}

// A name the namespace does not hold attaches no worker, and the reason lists
// the names it does hold, which is what a reviewer corrects the run with.
func TestPlanRefusesEveryWorkerWhenTheDefinitionIsMissing(t *testing.T) {
	plan := Planner{}.Plan(attachedFleet(3, dataDefinition, mgmtDefinition), attachedRun("storage-nope", ""))

	if len(plan.NodeSets) != 0 {
		t.Errorf("drafted %+v for a definition the namespace does not hold", plan.NodeSets)
	}
	refused := attachmentRefusals(plan)
	if len(refused) != 3 {
		t.Fatalf("refused %d workers by %q, want 3: %v", len(refused), NetworkAttachmentRule, plan.RefusalLines())
	}
	for _, refusal := range refused {
		if !strings.Contains(refusal.Reason, "storage-nope") || !strings.Contains(refusal.Reason, "storage-data, storage-mgmt") {
			t.Errorf("the reason %q does not name the missing definition and the ones there are", refusal.Reason)
		}
	}
}

func TestPlanRefusesAWorkerWithoutTheHostInterface(t *testing.T) {
	reports := attachedFleet(3, dataDefinition, mgmtDefinition)
	reports[1].Interfaces = reports[1].Interfaces[:1] // worker-2 has no eth1

	plan := Planner{}.Plan(reports, attachedRun("storage-mgmt", "storage-data"))

	group := onlyGroup(t, plan)
	if strings.Join(group.Workers, ",") != "worker-1,worker-3" {
		t.Errorf("the group holds %v, want the two workers with eth1", group.Workers)
	}
	refused := attachmentRefusals(plan)
	if len(refused) != 1 || refused[0].Worker != "worker-2" ||
		!strings.Contains(refused[0].Reason, "eth1") || !strings.Contains(refused[0].Reason, "storage-data") {
		t.Errorf("the refusals are %+v, want worker-2 for its missing eth1", refused)
	}
}

// A worker whose probe could not list the definitions cannot be checked, and a
// worker that cannot be checked is not proposed.
func TestPlanRefusesAWorkerWhoseAttachmentsWereNotRead(t *testing.T) {
	reports := attachedFleet(3, mgmtDefinition)
	reports[2].NetworkAttachments = &nodeprobe.NetworkAttachments{State: "Unread", Detail: "forbidden by RBAC"}

	plan := Planner{}.Plan(reports, attachedRun("storage-mgmt", ""))

	group := onlyGroup(t, plan)
	if strings.Join(group.Workers, ",") != "worker-1,worker-2" {
		t.Errorf("the group holds %v", group.Workers)
	}
	refused := attachmentRefusals(plan)
	if len(refused) != 1 || refused[0].Worker != "worker-3" || !strings.Contains(refused[0].Reason, "forbidden by RBAC") {
		t.Errorf("the refusals are %+v, want worker-3 with the reason the list failed", refused)
	}
}

// A bridge plugin creates its bridge when the host has none, and a definition
// that names no host interface is placed by the scheduler, so neither is held
// against a worker.
func TestPlanAdmitsDefinitionsItCannotCheckAgainstTheHost(t *testing.T) {
	bridge := nodeprobe.NetworkAttachment{Name: "storage-bridge", Type: "bridge", HostInterface: "br-storage"}
	sriov := nodeprobe.NetworkAttachment{Name: "storage-sriov", Type: "sriov", ResourceName: "intel.com/sriov"}

	plan := Planner{}.Plan(attachedFleet(2, bridge, sriov), attachedRun("storage-bridge", "storage-sriov"))

	if group := onlyGroup(t, plan); len(group.Workers) != 2 {
		t.Errorf("the group holds %v, want both workers", group.Workers)
	}
}

func hasFinding(plan Plan, reason string) bool {
	for _, found := range plan.Findings {
		if found.Reason == reason {
			return true
		}
	}
	return false
}

// attachmentRefusals is the refusals of the network attachment rule.
func attachmentRefusals(plan Plan) []Refusal {
	var out []Refusal
	for _, refusal := range plan.Refusals {
		if refusal.Rule == NetworkAttachmentRule {
			out = append(out, refusal)
		}
	}
	return out
}

// A namespace with no definitions at all says so, rather than listing none.
func TestPlanSaysTheNamespaceHoldsNoDefinitions(t *testing.T) {
	plan := Planner{}.Plan(attachedFleet(2), attachedRun("storage-mgmt", ""))

	refused := attachmentRefusals(plan)
	if len(refused) != 2 {
		t.Fatalf("refused %d workers, want 2: %v", len(refused), plan.RefusalLines())
	}
	for _, refusal := range refused {
		if strings.Contains(refusal.Reason, "only none") || !strings.Contains(refusal.Reason, "holds none") {
			t.Errorf("the reason %q does not say the namespace holds no definitions", refusal.Reason)
		}
	}
}
