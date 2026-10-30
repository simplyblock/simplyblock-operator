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
	"maps"
	"slices"
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

	// SystemReservedCPUs is what the draft keeps back from a memory node's
	// logical CPUs: the two the node preparation reserves for the kubelet and
	// the system (reservedSystemCPUs "0,1") where CPU topology is on. The SPDK
	// pod requests its whole core count, so a draft that keeps back less than
	// the reserve asks for more than the node's allocatable CPU and the pod
	// never schedules. It is also past the control plane's own floor of one
	// CPU beyond the count.
	SystemReservedCPUs = 2

	// DefaultMaxSubsystemCount is what a draft proposes for the number of
	// NVMe-oF subsystems each storage node serves.
	//
	// Nothing a probe reports bears on it: it is a function of how many volumes
	// the deployment expects to serve and how they are spread, which is a
	// question about the workload rather than about the hardware. The API
	// accepts 10 to 500, and this is a modest value within it, chosen so that
	// a reviewer who has not thought about it gets a working cluster without
	// the huge pages of a large one, and one who has can see the number was
	// not derived.
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

// TemplateOptions is what the run states about the cluster it proposes, beyond
// what the fleet says.
//
// It is a struct rather than more parameters because both members answer one
// question — what an installation decided that no probe could read — and a third
// positional argument at a call site that already passes two would read as an
// unlabeled list.
type TemplateOptions struct {
	// Seed is the cluster the installation stated, which the generator starts
	// from and fills in. Nil is an installation that stated nothing.
	Seed *simplyblockv1alpha2.ClusterTemplate

	// ForceJournalDevice makes the run dedicate a journal device even where the
	// fleet's disks do not say which one. journalDeviceFor is what it does, and
	// what it does not.
	ForceJournalDevice bool
}

// ClusterTemplateFor proposes the cluster for a plan.
//
// The vCPU count is the binding constraint and is taken from the smallest
// worker: the control plane assumes it uniform across a cluster's nodes, so a
// count that fits every worker is the only count that fits. Where the smallest
// worker cannot meet the API's floor the floor is used anyway and the note says
// so, because a draft that fails schema validation is one a reviewer cannot
// even read.
// It returns an error for a fleet it cannot draft at all. Only the journal
// device produces one today, and only for the logical block-device class: see
// journalDeviceFor.
func ClusterTemplateFor(name string, plan Plan, opts TemplateOptions) (ClusterTemplate, error) {
	out := ClusterTemplate{Template: &simplyblockv1alpha2.ClusterTemplate{Name: name}}

	seed := opts.Seed
	if seed == nil {
		seed = &simplyblockv1alpha2.ClusterTemplate{}
	}

	switch {
	case seed.Network != nil:
		out.Template.Network = seed.Network.DeepCopy()
		out.Notes = append(out.Notes, statedNote("network", "the one the installation stated"))
	case plan.DataNetworks > 0:
		out.Template.Network = hostNetwork()
		out.Notes = append(out.Notes, "network runs the SPDK pods on the host network, because most workers of the "+
			"draft have a data interface. Each group names its own NICs. A cluster's network is fixed when it is "+
			"created, so this is the line to correct now or never")
	case plan.Unattached:
		out.Template.Network = podNetworkOnly()
		out.Notes = append(out.Notes, "network is the pod network, because no worker has a data interface of its own. "+
			"A cluster's network is fixed when it is created, so this is the line to correct now or never")
	}

	if seed.EnableDriveFormat != nil {
		out.Template.EnableDriveFormat = seed.EnableDriveFormat
		out.Notes = append(out.Notes, statedNote("enableDriveFormat",
			formatting(*seed.EnableDriveFormat)))
	} else {
		out.Template.EnableDriveFormat = ptr.To(true)
		out.Notes = append(out.Notes,
			"enableDriveFormat is set, so every drive listed here is formatted before "+
				"a storage node takes it: a drive that carries anything is not usable "+
				"otherwise. This is the line to remove if any of them should be left "+
				"alone.")
	}

	// Not seeded, and deliberately: the count is taken from the smallest worker
	// the run found, because the control plane assumes it uniform across a
	// cluster's nodes, and the probes know the fleet better than an installation
	// decided before it had seen one.
	vcpus, note := vcpuCountFor(plan)
	out.Template.VCPUCount = ptr.To(vcpus)
	out.Notes = append(out.Notes, note)

	if seed.MaxSubsystemCount != nil {
		out.Template.MaxSubsystemCount = seed.MaxSubsystemCount
		out.Notes = append(out.Notes, statedNote("maxSubsystemCount",
			fmt.Sprintf("%d", *seed.MaxSubsystemCount)))
	} else {
		out.Template.MaxSubsystemCount = ptr.To(DefaultMaxSubsystemCount)
		out.Notes = append(out.Notes, fmt.Sprintf(
			"maxSubsystemCount is %d, which is the middle of the range this API accepts and "+
				"not a reading: how many subsystems a node should serve follows from the "+
				"workload rather than from the hardware", DefaultMaxSubsystemCount))
	}

	// Not seeded, for the reason vCPU count is not: it follows from what the
	// workers turned out to have.
	if size, note := hugePagesFor(plan); size != "" {
		out.Template.MinHugePagesSize = size
		out.Notes = append(out.Notes, note)
	}

	// Not seeded either: which disk becomes the journal follows from the sizes the
	// probes read, and a fleet without a small disk to spare has no such layout to
	// propose.
	propose, journalNote, err := journalDeviceFor(plan, opts.ForceJournalDevice)
	if err != nil {
		return ClusterTemplate{}, err
	}
	if journalNote != "" {
		if propose {
			out.Template.EnableJournalDevice = ptr.To(true)
		}
		out.Notes = append(out.Notes, journalNote)
	}

	if seed.Stripe != nil {
		out.Template.Stripe = seed.Stripe.DeepCopy()
		out.Notes = append(out.Notes, statedNote("stripe", describeStripe(seed.Stripe))+
			" A cluster's stripe cannot be changed afterward, so this is the number to "+
			"correct now or never.")
	} else {
		stripe, topology, note := stripeFor(plan, propose)
		out.Template.Stripe = stripe
		switch {
		case topology.SingleNode:
			out.Template.ClusterMode = simplyblockv1alpha2.ClusterModeSingle
		case topology.TwoNode:
			out.Template.ClusterMode = simplyblockv1alpha2.ClusterModeDual
		}
		out.Notes = append(out.Notes, note)
	}

	// The two integrity settings are stated or absent. Discovery proposes neither:
	// nothing a probe reports says whether a deployment wants its I/O checked, and
	// an absent setting leaves the cluster's own default to decide rather than
	// having the run invent one.
	if seed.EnableChecksumValidation != nil {
		out.Template.EnableChecksumValidation = seed.EnableChecksumValidation
		out.Notes = append(out.Notes, statedNote("enableChecksumValidation",
			enabled(*seed.EnableChecksumValidation))+
			" The backend bakes the checksum method into each device when the cluster "+
			"is created and never re-applies it, so a cluster created without this is "+
			"one nobody can turn it on for.")
	}
	if len(seed.SocketsToUse) > 0 {
		out.Template.SocketsToUse = slices.Clone(seed.SocketsToUse)
		out.Notes = append(out.Notes, statedNote("socketsToUse", strings.Join(seed.SocketsToUse, ", ")))
	}
	if seed.NodesPerSocket != nil {
		out.Template.NodesPerSocket = seed.NodesPerSocket
		out.Notes = append(out.Notes, statedNote("nodesPerSocket", fmt.Sprintf("%d", *seed.NodesPerSocket)))
	}
	if seed.EnableFailureDomains != nil {
		out.Template.EnableFailureDomains = seed.EnableFailureDomains
		out.Notes = append(out.Notes, statedNote("enableFailureDomains", enabled(*seed.EnableFailureDomains)))
	} else if plan.FailureDomains {
		if refused := failureDomainLayoutRefusal(plan, erasurecoding.SchemeOf(out.Template.Stripe)); refused != "" {
			out.Notes = append(out.Notes, "enableFailureDomains is off although every worker carries "+
				"a failure domain, because the control plane would refuse to activate the cluster: "+
				refused)
		} else {
			out.Template.EnableFailureDomains = ptr.To(true)
			out.Notes = append(out.Notes, "enableFailureDomains is on, because every worker carries a "+
				"failure domain, and each group names its workers' domain")
		}
	}
	if seed.EnableAtomicity4K != nil {
		out.Template.EnableAtomicity4K = seed.EnableAtomicity4K
		out.Notes = append(out.Notes, statedNote("enableAtomicity4K",
			enabled(*seed.EnableAtomicity4K))+
			" It is an enforcement rather than a reading: where the hardware does not "+
			"keep the guarantee, a torn write becomes a checksum that silently "+
			"disagrees with it. The device reports in this run are what to check it "+
			"against.")
	}

	return out, nil
}

// statedNote accounts for a value this installation decided, so that a reviewer
// reads which of the two kinds of number they are looking at. A derived number is
// corrected against the fleet, and a stated one against whoever stated it.
func statedNote(field, value string) string {
	return fmt.Sprintf(
		"%s is %s, stated by this installation's bootstrap configuration rather than "+
			"derived from what this run found.", field, value)
}

// failureDomainLayoutRefusal is why the control plane would refuse to activate
// a cluster of the plan in failure-domain mode, or "" when it would not. Each
// node set is a cluster of its own, so each is answered on its own.
func failureDomainLayoutRefusal(plan Plan, scheme erasurecoding.Scheme) string {
	for _, set := range plan.NodeSets {
		hosts := map[string]map[string]struct{}{}
		for _, group := range set.Groups {
			if hosts[group.FailureDomain] == nil {
				hosts[group.FailureDomain] = map[string]struct{}{}
			}
			for _, worker := range group.Workers {
				hosts[group.FailureDomain][worker] = struct{}{}
			}
		}
		layout := make(map[string]int, len(hosts))
		for domain, workers := range hosts {
			layout[domain] = len(workers)
		}
		if refused := erasurecoding.FailureDomainViolation(scheme, layout); refused != "" {
			if len(plan.NodeSets) > 1 {
				return fmt.Sprintf("node set %s: %s", set.Name, refused)
			}
			return refused
		}
	}
	return ""
}

// describeStripe reads a seeded layout back as a reviewer would write it, and
// says which half is missing where one is: a stripe naming one of its two numbers
// is a layout the cluster cannot be created with.
func describeStripe(stripe *simplyblockv1alpha2.StripeSpec) string {
	switch {
	case stripe.DataChunks == nil && stripe.ParityChunks == nil:
		return "empty"
	case stripe.ParityChunks == nil:
		return fmt.Sprintf("%d data chunks with no parity count stated", *stripe.DataChunks)
	case stripe.DataChunks == nil:
		return fmt.Sprintf("%d parity chunks with no data count stated", *stripe.ParityChunks)
	default:
		return fmt.Sprintf("%d+%d", *stripe.DataChunks, *stripe.ParityChunks)
	}
}

func formatting(on bool) string {
	if on {
		return "set, so every drive listed here is formatted before a storage node takes it"
	}
	return "unset, so a drive carrying anything is handed over as it is and a storage " +
		"node that cannot use it says so"
}

func enabled(on bool) string {
	if on {
		return "set"
	}
	return "unset"
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
//
// A fleet of one worker is proposed as a single-node cluster (clusterMode
// Single), and a fleet of exactly two as a two-node cluster (clusterMode Dual) running 1+1: without
// the declaration neither carries anything redundant, and the stripe a cluster
// is created with is the one it keeps. A single node places each chunk of a
// stripe on its own device, so its scheme follows from the devices the worker
// hands over for data (all of them, less the one a proposed journal device
// takes): three or more carry 2+1, two carry 1+1, and one carries only 1+0.
//
// A draft of several node sets is a supercluster, each node set a member that
// stripes across its own nodes alone (design-superclusters.md §5.1), so the
// ladder is walked for the smallest node set. The cluster mode is one per
// document until a node set can override it, so such a draft proposes neither
// single-node nor two-node mode: a one-worker tier would otherwise make every
// member single-node.
func stripeFor(plan Plan, journalDevice bool) (*simplyblockv1alpha2.StripeSpec, erasurecoding.Topology, string) {
	nodes := fleetSize(plan)
	members := len(plan.NodeSets) > 1
	if members {
		nodes = smallestNodeSet(plan)
	}

	var topology erasurecoding.Topology
	var note string
	scheme := erasurecoding.Scheme{DataChunks: 2, ParityChunks: 1}
	switch {
	case members && nodes < 3:
		scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 0}
		note = memberStripeNote(scheme, nodes)
	case members:
		if nodes < scheme.MinimumNodes() {
			scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 1}
		}
		note = memberStripeNote(scheme, nodes)
	case nodes == 1:
		devices := dataDevices(plan, journalDevice)
		switch {
		case devices >= 3:
			scheme = erasurecoding.Scheme{DataChunks: 2, ParityChunks: 1}
		case devices == 2:
			scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 1}
		default:
			scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 0}
		}
		topology.SingleNode = true
		note = singleNodeStripeNote(scheme, devices)
	case nodes == 2:
		scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 1}
		topology.TwoNode = true
		note = fmt.Sprintf(
			"stripe is %s as a two-node cluster (clusterMode Dual): one copy on each of the two "+
				"workers, so the cluster survives the loss of either; data migration after a "+
				"failure runs in relaxed placement, so it needs no third node. Outside clusterMode "+
				"Dual every redundant scheme needs at least three storage nodes. A cluster's stripe "+
				"cannot be changed afterward", scheme)
	case nodes < 3:
		scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 0}
		note = stripeNote(scheme, nodes)
	case nodes < scheme.MinimumNodes():
		scheme = erasurecoding.Scheme{DataChunks: 1, ParityChunks: 1}
		note = stripeNote(scheme, nodes)
	default:
		note = stripeNote(scheme, nodes)
	}

	stripe := &simplyblockv1alpha2.StripeSpec{
		DataChunks:   ptr.To(int32(scheme.DataChunks)),
		ParityChunks: ptr.To(int32(scheme.ParityChunks)),
	}
	return stripe, topology, note
}

// dataDevices is how many devices the fleet's one worker hands over for data:
// the disks the draft names, less the one a proposed journal device takes.
func dataDevices(plan Plan, journalDevice bool) int {
	if len(plan.Workers) != 1 {
		return 0
	}
	devices := len(plan.Workers[0].Addresses())
	if journalDevice && devices > 0 {
		devices--
	}
	return devices
}

// singleNodeStripeNote accounts for a single-node proposal: what the parity
// protects against, and which wider schemes the devices would also carry.
func singleNodeStripeNote(scheme erasurecoding.Scheme, devices int) string {
	if scheme.ParityChunks == 0 {
		return fmt.Sprintf(
			"stripe is %s as a single-node cluster (clusterMode Single), which protects nothing: "+
				"the worker hands over %d %s for data, and a redundant scheme places each "+
				"chunk of a stripe on its own device, so 1+1 needs two. A cluster's stripe "+
				"cannot be changed afterward",
			scheme, devices, plural(devices, "device", "devices"))
	}
	var alternatives []string
	for _, other := range erasurecoding.Supported() {
		if other == scheme || other.ParityChunks == 0 ||
			other.DataChunks+other.ParityChunks > devices {
			continue
		}
		alternatives = append(alternatives, other.String())
	}
	also := ""
	if len(alternatives) > 0 {
		also = fmt.Sprintf(" The devices would also carry %s, which trade capacity for a "+
			"second tolerated device failure or the other way about.",
			strings.Join(alternatives, ", "))
	}
	return fmt.Sprintf(
		"stripe is %s as a single-node cluster (clusterMode Single): each chunk of a stripe lands "+
			"on its own device of the one worker, which hands over %d for data, so the "+
			"cluster survives %d device %s but not the loss of the worker; data migration "+
			"after a device failure runs in relaxed placement on the same node.%s A cluster "+
			"meant to grow to more workers is not created as Single and states the stripe of the "+
			"fleet it will have. A cluster's stripe cannot be changed afterward",
		scheme, devices, scheme.ParityChunks, plural(scheme.ParityChunks, "failure", "failures"), also)
}

// stripeNote accounts for the proposal, which for this field means saying what
// the fleet rules out as well as what it allows.
func stripeNote(scheme erasurecoding.Scheme, nodes int) string {
	if scheme.ParityChunks == 0 {
		return fmt.Sprintf(
			"stripe is %s, which protects nothing: every redundant scheme needs at least "+
				"three storage nodes and this fleet has %d, so a third worker is what makes "+
				"1+1 possible; a fleet of exactly two can instead run 1+1 as a two-node "+
				"cluster (spec.cluster.clusterMode Dual). A cluster's stripe cannot be changed afterward",
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
// smallestNodeSet is the worker count of the draft's smallest node set.
func smallestNodeSet(plan Plan) int {
	smallest := 0
	for i, set := range plan.NodeSets {
		workers := map[string]struct{}{}
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				workers[worker] = struct{}{}
			}
		}
		if i == 0 || len(workers) < smallest {
			smallest = len(workers)
		}
	}
	return smallest
}

// memberStripeNote accounts for the proposal of a draft of several node sets.
func memberStripeNote(scheme erasurecoding.Scheme, nodes int) string {
	return fmt.Sprintf(
		"stripe is %s: the draft has several node sets, and each becomes a member cluster of a "+
			"supercluster that stripes across its own nodes alone, so the scheme is one the "+
			"smallest node set, of %d storage %s, carries. Delete the node sets you do not want, "+
			"or widen the stripe if every node set you keep carries more. A cluster's stripe "+
			"cannot be changed afterward",
		scheme, nodes, plural(nodes, "node", "nodes"))
}

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

// vcpuCountFor is the smallest chosen memory node's logical CPU count across the
// fleet less the system reserve, floored at the API's minimum. The control plane
// counts logical CPUs, refuses a node without one CPU beyond the cluster's vCPU
// count, and schedules the SPDK pod only within the node's allocatable CPU.
func vcpuCountFor(plan Plan) (int32, string) {
	smallest, smallestWorker := 0, ""
	for _, worker := range plan.Workers {
		cpus := chosenNodeCPUs(worker)
		if cpus == 0 {
			continue
		}
		// A worker too small for the reserve still counts: it is the smallest
		// placement, and the note names it rather than claiming no report.
		cores := cpus - SystemReservedCPUs
		if smallestWorker == "" || cores < smallest {
			smallest, smallestWorker = cores, worker.Name
		}
	}

	if smallestWorker == "" {
		return MinimumVCPUCount, fmt.Sprintf(
			"vcpuCount is the API's minimum of %d, because no worker reported the cores of "+
				"the memory node it was placed on", MinimumVCPUCount)
	}
	if int32(smallest) < MinimumVCPUCount {
		return MinimumVCPUCount, fmt.Sprintf(
			"vcpuCount is the API's minimum of %d, and the smallest placement (%s) has only "+
				"%d CPUs to spare for SPDK, so this cluster asks for more than that worker has",
			MinimumVCPUCount, smallestWorker, max(smallest, 0))
	}
	return int32(smallest), fmt.Sprintf(
		"vcpuCount is %d, the logical CPUs of the memory node chosen on %s less the %d the kubelet "+
			"keeps for the system, the smallest of the fleet: the control plane assumes this uniform across a cluster's nodes",
		smallest, smallestWorker, SystemReservedCPUs)
}

// chosenNodeCPUs is the logical CPUs of the memory node a worker's chosen
// devices hang off. A worker whose devices span nodes, which AllDevices
// produces, has no single answer and reports none.
func chosenNodeCPUs(worker Worker) int {
	node, single := chosenNode(worker)
	if !single {
		return 0
	}
	for _, cpus := range worker.Report.CPU.NUMANodes {
		if cpus.Node == node {
			return len(cpus.OnlineCPUs)
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

// journalDeviceFor decides whether the draft gives the journal manager a device
// of its own, and says why either way.
//
// The shape it reads is a worker whose smallest disk is smaller than every other
// disk it hands over. A fleet built that way was built that way on purpose: the
// odd disk is there to carry the journal, and saying nothing spends it as
// storage and carves a journal partition out of every disk instead, which is the
// layout the hardware was bought to avoid.
//
// It is unanimous across the fleet or it does not happen. The field is one value
// for the whole cluster and immutable once the cluster exists, so a fleet where
// one worker has the shape and another does not has no answer that is right for
// both, and the draft leaves it to the reviewer rather than picking one.
//
// A tie is not the shape. Two disks of the same smallest size are two disks the
// fleet can use, and giving one to the journal spends capacity nobody set aside:
// a worker with ten 10 TB disks would silently lose 10 TB of it. Neither is a
// worker with one disk, which would be left no storage at all, nor one reporting
// a disk of no size, because an unsized disk is smaller than anything and the
// generator invents those for claimed userspace controllers.
//
// The two classes part company on what happens then. An NVMe cluster with no
// dedicated journal device carves a journal partition out of every device, so
// leaving the field unset is a layout the backend builds and the fleet keeps
// every disk. A logical block-device cluster has no such fallback: the control
// plane refuses partitioned-journal mode for the class outright, so an unset
// field is a document that cannot deploy — every worker's node_add fails on it,
// after the cluster has been created and the drives formatted. A block run
// therefore refuses the fleet rather than drafting it, which is the same
// judgment made where it costs nothing.
//
// TemplateOptions.ForceJournalDevice overrides the tie, and only the tie. It
// breaks it by address, ascending, so two runs over one unchanged fleet name the
// same disk. It does not override the worker with one disk or the worker whose
// disks report no size: those are impossible rather than ambiguous, there is no
// answer to force, and forcing one would produce a storage node with nothing to
// store on.
//
// What counts as a disk here is what the draft names, which is a class address
// and not a probed device. An NVMe controller with two namespaces is two
// devices in the report and one entry in the document, so counting devices both
// over-counts a worker that has one controller and mistakes a namespace for the
// disk it sits on. The capacity compared is the controller's, summed across its
// namespaces, because that is the disk a reviewer is being asked to give up.
func journalDeviceFor(plan Plan, force bool) (bool, string, error) {
	if len(plan.Workers) == 0 {
		return false, "", nil
	}

	var smallest uint64
	var onWorker, named string
	forced := false

	for _, worker := range plan.Workers {
		size, address, verdict := smallestDisk(worker)

		switch {
		case verdict == journalDiskImpossible:
			// No answer to force. The worker hands over fewer than two disks, or
			// its disks report no size, and dedicating one either leaves it
			// nothing to store on or dedicates a disk nobody could measure.
			if plan.Class == ClassBlock {
				return false, "", fmt.Errorf(
					"%s hands over no disk that could carry the journal, and a %s cluster has "+
						"no other journal layout: the control plane refuses the partitioned "+
						"journal for this class, so a document leaving enableJournalDevice "+
						"unset creates the cluster, formats its drives, and then fails every "+
						"node_add. Give the worker a second disk, or deploy it as %s",
					worker.Name, ClassBlock, ClassNVMe)
			}
			return false, fmt.Sprintf(
				"enableJournalDevice is left unset: %s hands over no single disk smaller than "+
					"its others, so there is none to dedicate. Setting it would give up a disk "+
					"the fleet did not set aside, and the field cannot be changed once the "+
					"cluster exists", worker.Name), nil

		case verdict == journalDiskTied && !force:
			// The fleet declines to say which of several equal disks to take,
			// and nothing here decides for it.
			if plan.Class == ClassBlock {
				return false, "", fmt.Errorf(
					"%s hands over no disk smaller than its others, so nothing says which one "+
						"carries the journal, and a %s cluster has no other journal layout: "+
						"the control plane refuses the partitioned journal for this class, so "+
						"a document leaving enableJournalDevice unset creates the cluster, "+
						"formats its drives, and then fails every node_add. Set "+
						"spec.discover.forceJournalDevice to dedicate one of the equal disks "+
						"anyway, which spends %s of this worker's capacity on the journal",
					worker.Name, ClassBlock, humanBytes(size))
			}
			return false, fmt.Sprintf(
				"enableJournalDevice is left unset: %s hands over no single disk smaller than "+
					"its others, so there is none to dedicate. Setting it would give up a disk "+
					"the fleet did not set aside, and the field cannot be changed once the "+
					"cluster exists", worker.Name), nil

		case verdict == journalDiskTied:
			forced = true
		}

		if onWorker == "" || size < smallest {
			smallest, onWorker, named = size, worker.Name, address
		}
	}

	if forced {
		return true, fmt.Sprintf(
			"enableJournalDevice is set because spec.discover.forceJournalDevice asked for "+
				"it, not because the fleet said so: at least one worker hands over no disk "+
				"smaller than its others, and the draft takes one of the equal ones anyway. "+
				"The smallest named is %s (%s) on %s, and that whole disk carries the journal "+
				"instead of data. This is the line to remove if that capacity was meant to be "+
				"storage",
			humanBytes(smallest), named, onWorker), nil
	}

	return true, fmt.Sprintf(
		"enableJournalDevice is set: every worker hands over one disk smaller than its "+
			"others, the smallest being %s (%s) on %s, and the control plane dedicates that "+
			"disk to the journal manager instead of carving a journal partition out of every "+
			"disk. This is the line to remove if the disk was meant to carry data",
		humanBytes(smallest), named, onWorker), nil
}

// journalVerdict is what a worker's disks say about which one carries the
// journal, and it has three answers rather than two.
//
// The distinction is what a forced run turns on. A tie is the fleet declining to
// say which of several equal disks to take, and a run told to force it may pick
// one. A worker with one disk, or with disks of no size, is not ambiguous: there
// is no answer to force, because dedicating the only disk leaves the worker
// nothing to store on and an unsized disk is smaller than everything.
type journalVerdict int

const (
	// journalDiskFound: one disk is smaller than every other.
	journalDiskFound journalVerdict = iota

	// journalDiskTied: several disks share the smallest size, so the fleet does
	// not say which. This is the one a force resolves.
	journalDiskTied

	// journalDiskImpossible: the worker hands over fewer than two disks, or its
	// smallest disk reports no size. Neither is forceable.
	journalDiskImpossible
)

// smallestDisk returns the capacity and the draft's name of the disk a journal
// would go on, and which of the three answers the worker gave.
//
// It works in the addresses the draft names rather than the devices the probe
// reported, so a controller with two namespaces is one disk of their combined
// size. An address the class cannot name is skipped, which is the same device
// the draft would leave out.
//
// A tie is broken by address, ascending, so that two runs over one unchanged
// fleet name the same disk. Two drafts differing in which disk they give up is a
// diff nobody can account for, and the ordering costs nothing.
func smallestDisk(worker Worker) (uint64, string, journalVerdict) {
	capacity := map[string]uint64{}
	for _, device := range worker.Devices {
		address := worker.Class.Address(device)
		if address == "" {
			continue
		}
		capacity[address] += device.SizeBytes
	}
	if len(capacity) < 2 {
		return 0, "", journalDiskImpossible
	}

	var smallest uint64
	var found string
	ties := 0
	for _, address := range slices.Sorted(maps.Keys(capacity)) {
		size := capacity[address]
		switch {
		case found == "" || size < smallest:
			smallest, found, ties = size, address, 1
		case size == smallest:
			ties++
		}
	}

	if smallest == 0 {
		return 0, "", journalDiskImpossible
	}
	if ties != 1 {
		return smallest, found, journalDiskTied
	}
	return smallest, found, journalDiskFound
}

// plural picks the noun that agrees with n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// podNetworkOnly is the network of a cluster that serves data on the pod network.
func podNetworkOnly() *simplyblockv1alpha2.StorageNetworkSpec {
	return &simplyblockv1alpha2.StorageNetworkSpec{PodNetwork: true}
}

// hostNetwork is the network of a cluster whose SPDK pods bind host NICs. The NICs
// themselves are named by each group.
func hostNetwork() *simplyblockv1alpha2.StorageNetworkSpec {
	return &simplyblockv1alpha2.StorageNetworkSpec{SpdkOnHostNetwork: true}
}
