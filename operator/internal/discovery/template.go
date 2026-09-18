// The StorageCluster half of a draft, derived from what the fleet turned out to
// have.
//
// Two of the cluster's fields are required by the API and cannot be read off a
// worker, and this file is where that is dealt with honestly rather than by
// guessing quietly. The values below are starting points for a reviewer, and
// the sentence each one produces is what tells the reviewer it is one.
//
// A discovery run writes a Draft. Nothing here has to be right; it has to be
// plausible, stated, and easy to correct — which is the same standard the
// device list is held to, and the reason the approval gate exists at all.

package discovery

import (
	"fmt"
	"strings"

	"github.com/simplyblock/atlas/ptr"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"

	"github.com/simplyblock/simplyblock-operator/internal/erasurecoding"
)

const (
	// MinimumVCPUCount is the API's floor for a cluster's SPDK vCPU count. A
	// fleet whose chosen memory nodes have fewer cores than this cannot host a
	// storage node, and a draft naming a smaller number would be refused by
	// the schema rather than reviewed.
	MinimumVCPUCount int32 = 4

	// DefaultMaxSubsystemCount is what a draft proposes for the number of
	// NVMe-oF subsystems each storage node serves.
	//
	// Nothing a probe reports bears on it: it is a function of how many volumes
	// the deployment expects to serve and how they are spread, which is a
	// question about the workload rather than about the hardware. The API's
	// range is 10 to 75, and this is the middle of it, chosen so that a
	// reviewer who has not thought about it gets a working cluster and one who
	// has can see the number was not derived.
	DefaultMaxSubsystemCount int32 = 30
)

// ClusterTemplate is the cluster a draft proposes, and the sentences explaining
// where each derived number came from.
type ClusterTemplate struct {
	// Template is the API type, ready to go into a spec.
	Template *simplyblockv1alpha2.ClusterTemplate

	// Notes are one sentence per derived field, in the order a reviewer would
	// read them. They go into the run's status and its events, because a number
	// nobody can account for is a number nobody can correct with confidence.
	Notes []string
}

// ClusterTemplateFor proposes the cluster for a plan.
//
// The vCPU count is the binding constraint and is taken from the smallest
// worker: the control plane assumes it uniform across a cluster's nodes, so a
// count that fits every worker is the only count that fits. Where the smallest
// worker cannot meet the API's floor the floor is used anyway and the note says
// so, because a draft that fails schema validation is one a reviewer cannot
// even read.
func ClusterTemplateFor(name string, plan Plan) ClusterTemplate {
	out := ClusterTemplate{Template: &simplyblockv1alpha2.ClusterTemplate{
		Name:              name,
		MaxSubsystemCount: ptr.To(DefaultMaxSubsystemCount),
		EnableDriveFormat: ptr.To(true),
	}}

	out.Notes = append(out.Notes,
		"enableDriveFormat is set, so every drive listed here is formatted before "+
			"a storage node takes it: a drive that carries anything is not usable "+
			"otherwise. This is the line to remove if any of them should be left "+
			"alone.")

	vcpus, note := vcpuCountFor(plan)
	out.Template.VCPUCount = ptr.To(vcpus)
	out.Notes = append(out.Notes, note)

	out.Notes = append(out.Notes, fmt.Sprintf(
		"maxSubsystemCount is %d, which is the middle of the range this API accepts and "+
			"not a reading: how many subsystems a node should serve follows from the "+
			"workload rather than from the hardware", DefaultMaxSubsystemCount))

	if size, note := hugePagesFor(plan); size != "" {
		out.Template.MinHugePagesSize = size
		out.Notes = append(out.Notes, note)
	}

	stripe, note := stripeFor(plan)
	out.Template.Stripe = stripe
	out.Notes = append(out.Notes, note)
	return out
}

// stripeFor proposes the erasure-coding scheme for the fleet the run found.
//
// It is stated rather than left out, and that is the whole point of it. An
// unstated stripe is the control plane's 1+1, which needs three storage nodes,
// so a draft that says nothing about erasure coding proposes a scheme a fleet of
// one or two cannot carry — and says it in the one field a reviewer cannot
// correct after approval, since the cluster's stripe is immutable.
//
// The ladder is deliberately short. 2+1 is the widest stripe the product
// documentation recommends without knowing the workload, and it is proposed
// wherever the fleet meets its minimum; 1+1 is what a fleet of exactly three
// carries; 1+0 is what is left, and it protects nothing, which the note says in
// those words. Everything beyond that — the tolerance of two failures that 1+2,
// 2+2, and 4+2 buy, and the capacity 4+1 saves — is a trade the reviewer makes
// against a workload this run knows nothing about, so the note names the
// alternatives rather than picking one.
func stripeFor(plan Plan) (*simplyblockv1alpha2.StripeSpec, string) {
	nodes := fleetSize(plan)

	scheme := erasurecoding.Scheme{DataChunks: 2, ParityChunks: 1}
	switch {
	case nodes < 3:
		scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 0}
	case nodes < scheme.MinimumNodes():
		scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 1}
	}

	stripe := &simplyblockv1alpha2.StripeSpec{
		DataChunks:   ptr.To(int32(scheme.DataChunks)),
		ParityChunks: ptr.To(int32(scheme.ParityChunks)),
	}
	return stripe, stripeNote(scheme, nodes)
}

// stripeNote accounts for the proposal, which for this field means saying what
// the fleet rules out as well as what it allows.
func stripeNote(scheme erasurecoding.Scheme, nodes int) string {
	if scheme.ParityChunks == 0 {
		return fmt.Sprintf(
			"stripe is %s, which protects nothing: every redundant scheme needs at least "+
				"three storage nodes and this fleet has %d, so a third worker is what makes "+
				"1+1 possible. A cluster's stripe cannot be changed afterward",
			scheme, nodes)
	}

	alternatives := alternativesFor(scheme, nodes)
	if len(alternatives) == 0 {
		return fmt.Sprintf(
			"stripe is %s, the only redundant scheme a fleet of %d carries: it needs %d "+
				"storage nodes and the next scheme up needs more. A cluster's stripe cannot "+
				"be changed afterward",
			scheme, nodes, scheme.MinimumNodes())
	}
	return fmt.Sprintf(
		"stripe is %s, which needs %d storage nodes and this fleet of %d has them: it is "+
			"the widest stripe to propose without knowing the workload. This fleet could "+
			"also carry %s, which trade capacity for a second tolerated failure or the "+
			"other way about, and a cluster's stripe cannot be changed afterward",
		scheme, scheme.MinimumNodes(), nodes, strings.Join(alternatives, ", "))
}

// alternativesFor is every other supported scheme the fleet is large enough for,
// in the documentation's order.
func alternativesFor(chosen erasurecoding.Scheme, nodes int) []string {
	var out []string
	for _, scheme := range erasurecoding.Supported() {
		if scheme == chosen || scheme.ParityChunks == 0 || scheme.MinimumNodes() > nodes {
			continue
		}
		out = append(out, scheme.String())
	}
	return out
}

// fleetSize is how many storage nodes the draft produces, which is one per
// worker: the template proposes neither socketsToUse nor nodesPerSocket, so
// every worker runs a single node.
func fleetSize(plan Plan) int {
	workers := map[string]struct{}{}
	for _, set := range plan.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				workers[worker] = struct{}{}
			}
		}
	}
	return len(workers)
}

// vcpuCountFor is the smallest chosen memory node's core count across the
// fleet, floored at the API's minimum.
func vcpuCountFor(plan Plan) (int32, string) {
	smallest, smallestWorker := 0, ""
	for _, worker := range plan.Workers {
		cores := chosenNodeCores(worker)
		if cores == 0 {
			continue
		}
		if smallest == 0 || cores < smallest {
			smallest, smallestWorker = cores, worker.Name
		}
	}

	if smallest == 0 {
		return MinimumVCPUCount, fmt.Sprintf(
			"vcpuCount is the API's minimum of %d, because no worker reported the cores of "+
				"the memory node it was placed on", MinimumVCPUCount)
	}
	if int32(smallest) < MinimumVCPUCount {
		return MinimumVCPUCount, fmt.Sprintf(
			"vcpuCount is the API's minimum of %d, and the smallest placement (%s) has only "+
				"%d cores, so this cluster asks for more than that worker has",
			MinimumVCPUCount, smallestWorker, smallest)
	}
	return int32(smallest), fmt.Sprintf(
		"vcpuCount is %d, the cores of the memory node chosen on %s, which is the smallest "+
			"of the fleet: the control plane assumes this uniform across a cluster's nodes",
		smallest, smallestWorker)
}

// chosenNodeCores is the physical cores of the memory node a worker's chosen
// devices hang off. A worker whose devices span nodes, which AllDevices
// produces, has no single answer and reports none.
func chosenNodeCores(worker Worker) int {
	node, single := chosenNode(worker)
	if !single {
		return 0
	}
	for _, cpus := range worker.Report.CPU.NUMANodes {
		if cpus.Node == node {
			return cpus.PhysicalCores
		}
	}
	return 0
}

// chosenNode is the memory node every chosen device is on, and whether there is
// exactly one.
func chosenNode(worker Worker) (int, bool) {
	if len(worker.Devices) == 0 {
		return 0, false
	}
	node := worker.Devices[0].NUMANode
	for _, device := range worker.Devices[1:] {
		if device.NUMANode != node {
			return 0, false
		}
	}
	return node, true
}

// hugePagesFor proposes the smallest huge-page allocation the fleet can meet,
// and proposes nothing where a worker has none reserved.
//
// Nothing is the right answer there rather than a number: SPDK consumes huge
// pages and does not reserve them, so a draft naming an allocation no worker
// has would describe a cluster that cannot start, and leaving the field unset
// lets each node compute its own minimum.
func hugePagesFor(plan Plan) (string, string) {
	smallest, smallestWorker := uint64(0), ""
	for _, worker := range plan.Workers {
		free := chosenNodeHugePageBytes(worker)
		if free == 0 {
			return "", fmt.Sprintf(
				"minHugePagesSize is unset, because %s has no huge pages reserved on the "+
					"memory node it was placed on: SPDK consumes them and does not reserve "+
					"them, so each node computes its own minimum until the fleet is prepared",
				worker.Name)
		}
		if smallest == 0 || free < smallest {
			smallest, smallestWorker = free, worker.Name
		}
	}
	if smallest == 0 {
		return "", ""
	}

	gigabytes := smallest >> 30
	if gigabytes == 0 {
		return "", fmt.Sprintf(
			"minHugePagesSize is unset, because the smallest reservation in the fleet (%s on "+
				"%s) is under a gigabyte", humanBytes(smallest), smallestWorker)
	}
	return fmt.Sprintf("%dG", gigabytes), fmt.Sprintf(
		"minHugePagesSize is %dG, the huge pages free on the memory node chosen on %s, which "+
			"is the smallest of the fleet", gigabytes, smallestWorker)
}

// chosenNodeHugePageBytes is the huge-page memory free on the node a worker's
// devices hang off.
func chosenNodeHugePageBytes(worker Worker) uint64 {
	node, single := chosenNode(worker)
	if !single {
		return 0
	}
	var free uint64
	for _, pool := range worker.Report.HugePages {
		for _, share := range pool.NUMANodes {
			if share.Node == node {
				free += share.Free * pool.SizeBytes
			}
		}
	}
	return free
}
