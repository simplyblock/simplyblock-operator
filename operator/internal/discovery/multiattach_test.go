// What a discovery run drafts for a multi-attach deployment: the run was told
// the storage array up front, so the draft carries it, names one local device
// per node (its journal, the smallest disk), and says which workers have no
// initiator for the array's protocol.

package discovery

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

func blockWorker(name string, initiators *nodeprobe.SANInitiators, sizes ...uint64) Worker {
	devices := make([]nodeprobe.Device, 0, len(sizes))
	for i, size := range sizes {
		d := disk("sd"+string(rune('a'+i)), "", 0, size)
		d.StablePath = "/dev/disk/by-id/wwn-" + string(rune('a'+i))
		devices = append(devices, d)
	}
	return Worker{Name: name, Class: ClassBlock, Devices: devices,
		Report: nodeprobe.Report{Node: name, SANInitiators: initiators}}
}

func arrayPlan(workers ...Worker) Plan {
	plan := Plan{Class: ClassBlock, Workers: workers}
	group := simplyblockv1alpha2.NodeGroup{Name: "group-1",
		Devices: &simplyblockv1alpha2.DeviceSelection{}}
	for _, w := range workers {
		group.Workers = append(group.Workers, w.Name)
	}
	for _, d := range workers[0].Devices {
		group.Devices.Block = append(group.Devices.Block, ClassBlock.Address(d))
	}
	plan.NodeSets = []simplyblockv1alpha2.NodeSet{{Name: "discovered",
		Groups: []simplyblockv1alpha2.NodeGroup{group}}}
	return plan
}

func iscsiArray() *simplyblockv1alpha2.MultiAttachSpec {
	return &simplyblockv1alpha2.MultiAttachSpec{
		FaultTolerance: 1,
		Array: &simplyblockv1alpha2.StorageArraySpec{
			Plugin: "pure-flasharray", Endpoint: "https://pure01.example",
			Protocol: simplyblockv1alpha2.StorageArrayProtocolISCSI,
			LunSize:  resource.MustParse("4Ti"),
			Credentials: simplyblockv1alpha2.StorageArrayCredentials{
				Source:  simplyblockv1alpha2.StorageArrayCredentialsSourceVaultKV,
				VaultKV: &simplyblockv1alpha2.StorageArrayVaultKVSecret{Mount: "secret", Path: "arrays/pure01"},
			},
		},
	}
}

func TestAMultiAttachDraftCarriesTheArrayAndOneJournalPerNode(t *testing.T) {
	iqn := &nodeprobe.SANInitiators{ISCSIIQN: "iqn.x"}
	plan := arrayPlan(
		blockWorker("worker-1", iqn, 960*gib, 120*gib),
		blockWorker("worker-2", iqn, 960*gib, 120*gib),
	)
	template := &simplyblockv1alpha2.ClusterTemplate{EnableJournalDevice: boolPtr(true)}

	notes := ApplyMultiAttach(&plan, template, iscsiArray())

	if template.MultiAttach == nil || template.MultiAttach.Array.Plugin != "pure-flasharray" ||
		template.MultiAttach.Array.Credentials.VaultKV.Path != "arrays/pure01" {
		t.Fatalf("template.multiAttach = %+v", template.MultiAttach)
	}
	if template.RaidDevices == nil || !*template.RaidDevices || template.EnableJournalDevice != nil {
		t.Errorf("raidDevices %v, enableJournalDevice %v; want true and unset",
			template.RaidDevices, template.EnableJournalDevice)
	}
	got := plan.NodeSets[0].Groups[0].Devices.Block
	if len(got) != 1 || got[0] != "/dev/disk/by-id/wwn-b" {
		t.Errorf("group devices = %v, want the smallest disk only", got)
	}
	if len(plan.Findings) != 0 {
		t.Errorf("findings = %+v", plan.Findings)
	}
	if !strings.Contains(strings.Join(notes, " "), "journal") {
		t.Errorf("notes do not explain the journal device: %v", notes)
	}
}

func TestAWorkerWithoutAnInitiatorForTheProtocolIsNamed(t *testing.T) {
	plan := arrayPlan(
		blockWorker("worker-1", &nodeprobe.SANInitiators{ISCSIIQN: "iqn.x"}, 120*gib),
		blockWorker("worker-2", &nodeprobe.SANInitiators{NVMeHostNQN: "nqn.y"}, 120*gib),
		blockWorker("worker-3", nil, 120*gib),
	)
	ApplyMultiAttach(&plan, &simplyblockv1alpha2.ClusterTemplate{}, iscsiArray())

	if len(plan.Findings) != 1 || plan.Findings[0].Reason != ArrayInitiatorMissing {
		t.Fatalf("findings = %+v, want one %s", plan.Findings, ArrayInitiatorMissing)
	}
	msg := plan.Findings[0].Message
	for _, want := range []string{"worker-2", "worker-3", "iscsi"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the finding does not mention %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "worker-1") {
		t.Errorf("worker-1 has an IQN and is named: %s", msg)
	}
}

func TestTheProtocolDecidesWhichInitiatorCounts(t *testing.T) {
	for _, tc := range []struct {
		protocol simplyblockv1alpha2.StorageArrayProtocol
		have     nodeprobe.SANInitiators
		missing  bool
	}{
		{simplyblockv1alpha2.StorageArrayProtocolFC, nodeprobe.SANInitiators{FCWWPNs: []string{"21"}}, false},
		{simplyblockv1alpha2.StorageArrayProtocolFC, nodeprobe.SANInitiators{ISCSIIQN: "iqn"}, true},
		{simplyblockv1alpha2.StorageArrayProtocolNVMeTCP, nodeprobe.SANInitiators{NVMeHostNQN: "nqn"}, false},
		{simplyblockv1alpha2.StorageArrayProtocolNVMeFC, nodeprobe.SANInitiators{NVMeHostNQN: "nqn"}, false},
		// PowerFlex's SDC registers itself, and the probe cannot read its GUID.
		{simplyblockv1alpha2.StorageArrayProtocolSDC, nodeprobe.SANInitiators{}, false},
	} {
		have := tc.have
		plan := arrayPlan(blockWorker("worker-1", &have, 120*gib))
		ma := iscsiArray()
		ma.Array.Protocol = tc.protocol
		ApplyMultiAttach(&plan, &simplyblockv1alpha2.ClusterTemplate{}, ma)
		if missing := len(plan.Findings) == 1; missing != tc.missing {
			t.Errorf("%s with %+v: missing = %v, want %v", tc.protocol, tc.have, missing, tc.missing)
		}
	}
}

// A run that found NVMe devices cannot draft a multi-attach cluster, which
// needs block devices: it says so rather than drafting one the schema refuses.
func TestAMultiAttachRunOnNVMeDevicesSaysSo(t *testing.T) {
	plan := arrayPlan(blockWorker("worker-1", &nodeprobe.SANInitiators{ISCSIIQN: "iqn"}, 120*gib))
	plan.Class = ClassNVMe
	ApplyMultiAttach(&plan, &simplyblockv1alpha2.ClusterTemplate{}, iscsiArray())
	if len(plan.Findings) != 1 || plan.Findings[0].Reason != MultiAttachNeedsBlockDevices {
		t.Fatalf("findings = %+v", plan.Findings)
	}
}

func boolPtr(b bool) *bool { return &b }
