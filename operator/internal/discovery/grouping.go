// How workers become groups, and groups become node sets.
//
// A NodeGroup's device selection is shared by every worker in it — that is what
// makes ten identical machines one entry rather than ten — so grouping is not a
// presentation choice. Two workers belong in one group when they hand over the
// same devices at the same addresses, and putting two workers with different
// addressing in one group claims each has the other's disks.
//
// That is why the simplest grouper here is not one group holding everything. A
// single group would be simpler and would be false, so the floor is grouping by
// what the workers actually have.
//
// Node sets are the other half and they are genuinely unanswerable today. A
// node set is a rack — the workers a document adds or grows together — and
// nothing in a probe report says which rack a worker is in. So there is one,
// and the seam is here for when the operator learns about topology.

package discovery

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// Worker is one machine that will be in the draft, with the decisions already
// made about it.
type Worker struct {
	// Name is the Kubernetes node name, which is what a NodeGroup lists.
	Name string

	// Report is what the probe found, kept so that a grouper can look at
	// anything it needs without the pipeline having to anticipate what.
	Report nodeprobe.Report

	// Devices are the devices chosen for this worker, in the order the draft
	// will name them.
	Devices []nodeprobe.Device

	// Class is how those devices are named.
	Class DeviceClass

	// PlacementReason is what the placement decided and why, for the record.
	PlacementReason string
}

// Addresses is how the draft names this worker's devices, ascending and without
// repeats.
//
// A repeat is possible and is not a mistake: an NVMe controller with two
// namespaces reports two devices at one PCI address, and the draft names the
// controller once.
func (w Worker) Addresses() []string {
	seen := map[string]struct{}{}
	addresses := make([]string, 0, len(w.Devices))
	for _, device := range w.Devices {
		address := w.Class.Address(device)
		if address == "" {
			continue
		}
		if _, repeat := seen[address]; repeat {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	slices.Sort(addresses)
	return addresses
}

// Group is a set of workers that share one configuration.
type Group struct {
	// Name identifies the group within its node set.
	Name string

	// Workers are the machines in it, ascending by name.
	Workers []Worker

	// Class and Addresses are the device selection they share.
	Class     DeviceClass
	Addresses []string
}

// Grouper puts workers into groups.
type Grouper interface {
	Name() string

	// Group returns the groups, which between them hold every worker given: a
	// grouper that dropped one would silently shrink the deployment.
	Group(workers []Worker) []Group
}

// GroupByHardware puts workers that hand over the same devices at the same
// addresses into one group.
//
// The signature is the address list, so a fleet of identical machines is one
// group and a fleet where one worker has a disk in a different slot is two.
// That is a guess at intent rather than a reading of it — two racks of
// identical machines are one group by this rule and two by any operational one
// — and it is the guess a reviewer regroups.
type GroupByHardware struct{}

func (GroupByHardware) Name() string { return "identical hardware" }

func (GroupByHardware) Group(workers []Worker) []Group {
	bySignature := map[string]*Group{}
	order := []string{}

	for _, worker := range workers {
		addresses := worker.Addresses()
		signature := worker.Class.signature(addresses)

		group, seen := bySignature[signature]
		if !seen {
			group = &Group{Class: worker.Class, Addresses: addresses}
			bySignature[signature] = group
			order = append(order, signature)
		}
		group.Workers = append(group.Workers, worker)
	}

	groups := make([]Group, 0, len(order))
	for _, signature := range order {
		group := bySignature[signature]
		slices.SortFunc(group.Workers, func(a, b Worker) int { return cmp.Compare(a.Name, b.Name) })
		groups = append(groups, *group)
	}

	// Ascending by the first worker's name, so two runs against one fleet
	// produce the groups in the same order and their generated names are
	// stable.
	slices.SortFunc(groups, func(a, b Group) int {
		return cmp.Compare(a.Workers[0].Name, b.Workers[0].Name)
	})
	for i := range groups {
		groups[i].Name = groupName(i, groups[i])
	}
	return groups
}

// signature is the key two workers must agree on to share a group: the class
// and the addresses, hashed so that a hundred addresses do not become a
// hundred-element map key.
func (c DeviceClass) signature(addresses []string) string {
	digest := sha256.Sum256([]byte(string(c) + "\x00" + strings.Join(addresses, "\x00")))
	return hex.EncodeToString(digest[:])
}

// groupName names a group for a reader.
//
// It is positional rather than derived from the hardware, because the name is
// what a reviewer edits and "group-1" invites that where a hash does not. The
// ordering above is what makes the number stable.
func groupName(index int, group Group) string {
	return fmt.Sprintf("group-%d-%s-%dx%s", index+1, group.Class,
		len(group.Addresses), humanBytes(groupDeviceBytes(group)))
}

// groupDeviceBytes is the size of one worker's devices in the group, which is
// the same for every worker in it by construction.
func groupDeviceBytes(group Group) uint64 {
	if len(group.Workers) == 0 {
		return 0
	}
	var total uint64
	for _, device := range group.Workers[0].Devices {
		total += device.SizeBytes
	}
	if len(group.Addresses) == 0 {
		return 0
	}
	return total / uint64(len(group.Addresses))
}

// NodeSetBuilder organizes groups into the node sets of a draft.
type NodeSetBuilder interface {
	Name() string
	Build(groups []Group) []simplyblockv1alpha1.NodeSet
}

// SingleNodeSet puts every group into one node set.
//
// A node set is a rack, and nothing a probe reports says which rack a worker is
// in: the Kubernetes API carries a zone label on some fleets and nothing on
// most, and a zone is not a rack. So one set, named for what it is, and a
// reviewer who knows the racks splits it.
type SingleNodeSet struct {
	// SetName is the node set's name. Empty is DefaultNodeSetName.
	SetName string
}

// DefaultNodeSetName is what the single node set is called. It says what the
// set is rather than pretending to a topology, so that a reviewer who does know
// the racks can see there is nothing to preserve in renaming it.
const DefaultNodeSetName = "discovered"

func (SingleNodeSet) Name() string { return "single node set" }

func (b SingleNodeSet) Build(groups []Group) []simplyblockv1alpha1.NodeSet {
	if len(groups) == 0 {
		return nil
	}

	name := b.SetName
	if name == "" {
		name = DefaultNodeSetName
	}

	set := simplyblockv1alpha1.NodeSet{Name: name, Groups: make([]simplyblockv1alpha1.NodeGroup, 0, len(groups))}
	for _, group := range groups {
		set.Groups = append(set.Groups, nodeGroupOf(group))
	}
	return []simplyblockv1alpha1.NodeSet{set}
}

// nodeGroupOf renders one group as the API's NodeGroup.
func nodeGroupOf(group Group) simplyblockv1alpha1.NodeGroup {
	workers := make([]string, 0, len(group.Workers))
	for _, worker := range group.Workers {
		workers = append(workers, worker.Name)
	}

	out := simplyblockv1alpha1.NodeGroup{Name: group.Name, Workers: workers}
	if len(group.Addresses) > 0 {
		selection := &simplyblockv1alpha1.DeviceSelection{}
		if group.Class == ClassBlock {
			selection.Block = group.Addresses
		} else {
			selection.NVMe = group.Addresses
		}
		out.Devices = selection
	}
	return out
}
