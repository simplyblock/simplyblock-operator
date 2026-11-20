// The draft of a multi-attach deployment: a cluster on LUNs of an external
// storage array.
//
// Its data devices cannot be discovered, because they do not exist yet: the
// control plane creates each node's LUN on the array when the node is added,
// presents it, and adopts it. What a run can read and settle beforehand is the
// rest: the array the run was told about goes into the draft, every node names
// one local device (its journal, whole), and a worker the array could never
// present a LUN to, for want of an initiator in the array's protocol, is named
// before anybody approves the document.

package discovery

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

const (
	// ArrayInitiatorMissing names the workers that report no initiator for the
	// storage array's protocol, so the array cannot present them a LUN.
	ArrayInitiatorMissing = "ArrayInitiatorMissing"

	// MultiAttachNeedsBlockDevices is a multi-attach run that found NVMe
	// devices: the cluster's journals are logical block devices.
	MultiAttachNeedsBlockDevices = "MultiAttachNeedsBlockDevices"
)

// ApplyMultiAttach turns a drafted plan and template into a multi-attach
// draft for the array ma names. It returns the notes the run reports, and
// records findings on the plan.
func ApplyMultiAttach(plan *Plan, template *simplyblockv1alpha2.ClusterTemplate,
	ma *simplyblockv1alpha2.MultiAttachSpec) []string {
	if ma == nil {
		return nil
	}
	var notes []string
	template.MultiAttach = ma.DeepCopy()
	template.RaidDevices = ptr.To(true)
	template.EnableJournalDevice = nil
	notes = append(notes, fmt.Sprintf(
		"multiAttach is set as this run was told: the cluster runs on LUNs of a %s array, "+
			"created and presented to each node when it is added, and tolerates %d node "+
			"failure(s); raidDevices is set because the array protects every LUN, and the stated "+
			"stripe does not apply: a multi-attach cluster runs 1+0",
		arrayPlugin(ma), ma.FaultTolerance))

	if plan.Class != ClassBlock {
		plan.Findings = append(plan.Findings, Finding{
			Reason: MultiAttachNeedsBlockDevices,
			Message: "a multi-attach cluster names its nodes' journals as logical block devices, " +
				"and this run found NVMe devices; run it again with enableLogicalBlockDevices",
		})
		return notes
	}

	slots := ptr.IntFrom(template.NodesPerSocket, 1)
	byName := map[string]Worker{}
	for _, w := range plan.Workers {
		byName[w.Name] = w
	}
	for s := range plan.NodeSets {
		for g := range plan.NodeSets[s].Groups {
			group := &plan.NodeSets[s].Groups[g]
			if group.Devices == nil || len(group.Devices.Block) <= slots || len(group.Workers) == 0 {
				continue
			}
			group.Devices.Block = smallestAddresses(byName[group.Workers[0]], group.Devices.Block, slots)
		}
	}
	notes = append(notes,
		"every group names its smallest local device per storage node, as the node's journal: "+
			"a multi-attach node's data device is its LUN on the array, so the other disks are "+
			"left alone. This is the line to correct if another disk should carry the journal")

	if missing := workersWithoutInitiator(plan.Workers, ma); len(missing) > 0 {
		plan.Findings = append(plan.Findings, Finding{
			Reason: ArrayInitiatorMissing,
			Message: fmt.Sprintf(
				"%s %s no %s initiator configured, so the array cannot present %s a LUN; "+
					"configure the initiator on the host (%s) before approving",
				strings.Join(missing, ", "), plural(len(missing), "reports", "report"),
				protocolOf(ma), plural(len(missing), "it", "them"), initiatorHint(protocolOf(ma))),
		})
	}
	return notes
}

func arrayPlugin(ma *simplyblockv1alpha2.MultiAttachSpec) string {
	if ma.Array == nil || ma.Array.Plugin == "" {
		return "storage"
	}
	return ma.Array.Plugin
}

func protocolOf(ma *simplyblockv1alpha2.MultiAttachSpec) simplyblockv1alpha2.StorageArrayProtocol {
	if ma.Array == nil {
		return ""
	}
	return ma.Array.Protocol
}

// smallestAddresses keeps the n smallest of the worker's devices among
// addresses, in the order the draft listed them.
func smallestAddresses(worker Worker, addresses []string, n int) []string {
	size := map[string]uint64{}
	for _, device := range worker.Devices {
		size[worker.Class.Address(device)] += device.SizeBytes
	}
	ranked := slices.Clone(addresses)
	slices.SortStableFunc(ranked, func(a, b string) int {
		switch {
		case size[a] < size[b]:
			return -1
		case size[a] > size[b]:
			return 1
		}
		return strings.Compare(a, b)
	})
	keep := map[string]bool{}
	for _, a := range ranked[:n] {
		keep[a] = true
	}
	var out []string
	for _, a := range addresses {
		if keep[a] {
			out = append(out, a)
		}
	}
	return out
}

// workersWithoutInitiator names the workers with no initiator for the
// array's protocol. PowerFlex's SDC registers itself with the array, and the
// probe has no initiator to read for it, so it names nobody.
func workersWithoutInitiator(workers []Worker, ma *simplyblockv1alpha2.MultiAttachSpec) []string {
	protocol := protocolOf(ma)
	if protocol == "" || protocol == simplyblockv1alpha2.StorageArrayProtocolSDC {
		return nil
	}
	missing := map[string]bool{}
	for _, w := range workers {
		if !hasInitiator(w.Report.SANInitiators, protocol) {
			missing[w.Name] = true
		}
	}
	return slices.Sorted(maps.Keys(missing))
}

func hasInitiator(in *nodeprobe.SANInitiators, protocol simplyblockv1alpha2.StorageArrayProtocol) bool {
	if in == nil {
		return false
	}
	switch protocol {
	case simplyblockv1alpha2.StorageArrayProtocolISCSI:
		return in.ISCSIIQN != ""
	case simplyblockv1alpha2.StorageArrayProtocolFC:
		return len(in.FCWWPNs) > 0
	default:
		return in.NVMeHostNQN != ""
	}
}

func initiatorHint(protocol simplyblockv1alpha2.StorageArrayProtocol) string {
	switch protocol {
	case simplyblockv1alpha2.StorageArrayProtocolISCSI:
		return "/etc/iscsi/initiatorname.iscsi, and sessions to the array's portals"
	case simplyblockv1alpha2.StorageArrayProtocolFC:
		return "an FC HBA with its ports zoned to the array"
	default:
		return "/etc/nvme/hostnqn, and connections to the array's subsystems"
	}
}
