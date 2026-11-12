// What a document's erasure coding is answered against, and the counting the
// answer needs.
//
// The scheme decides how many storage nodes the deployment must have: ndcs+npcs
// of them to place a stripe across, plus one spare per tolerated failure for the
// rebuild to land on. Nothing below this operator checks it. The control plane
// validates the scheme itself on the cluster create and counts devices at
// activation, never nodes, so a four-worker fleet configured 4+2 activates, and
// what it has bought is a cluster that loses data on the second failure it was
// configured to survive.
//
// The same scheme decides what those nodes hand over: at least one data device
// each, ndcs+2*npcs data devices across the cluster (ndcs+npcs when the
// cluster declares raidDevices, since a RAID-protected device absorbs a disk
// failure without costing the stripe a chunk), and, where the groups declare
// failure domains, ndcs+npcs distinct domains to spread a stripe's chunks over.
// A journal device is one per storage node and carries no data, so it is
// counted out first.
//
// Both callers of this file are here for the same reason the rest of the
// document's validation has two: a draft is told on every reconcile so it can be
// edited, and the approving edit is refused because §3.2 makes an approved
// document immutable. StripeChecks is therefore exported and takes a reader
// rather than a reconciler, and the two callers differ only in what they wrap the
// result in.
//
// The counting is by slot rather than by worker because the expansion creates
// nodes by slot: a growth document naming a worker the cluster already has a node
// on produces nothing there, and counting that worker again would report a
// minimum as met by a node nobody is going to create.

package deployment

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/erasurecoding"
)

// StripeCheck is one thing wrong with what a document says about erasure
// coding, carrying the reason it is counted and reported under as well as the
// sentence somebody reads.
type StripeCheck struct {
	Reason  string
	Message string
}

// footprint is a set of storage nodes, identified the way the expansion
// identifies them, and the workers they sit on. Both numbers are needed: the
// minimum is a node count, and a node count reached by running several nodes on
// one worker has not bought the independent spare the count was asking for.
type footprint struct {
	slots   map[slotKey]struct{}
	workers map[string]struct{}
}

func newFootprint() footprint {
	return footprint{slots: map[slotKey]struct{}{}, workers: map[string]struct{}{}}
}

func (f footprint) add(worker string, slot int32) {
	f.slots[slotKey{worker: worker, slot: slot}] = struct{}{}
	f.workers[worker] = struct{}{}
}

func (f footprint) merge(other footprint) {
	for key := range other.slots {
		f.slots[key] = struct{}{}
	}
	for worker := range other.workers {
		f.workers[worker] = struct{}{}
	}
}

// nodes is how many storage nodes the deployment ends up with.
func (f footprint) nodes() int { return len(f.slots) }

// hosts is how many workers those nodes sit on.
func (f footprint) hosts() int { return len(f.workers) }

// StripeChecks answers a document's erasure coding against the deployment it
// describes.
//
// It returns at most one check, unlike the validation it feeds, because the
// things that can be wrong here are one thing each: a scheme the control plane
// refuses has no minimum to be short of, and a deployment short of the node count
// is not also separately short of the worker count, the device count, or the
// failure-domain count that follow it. Reporting two of them would put two
// findings on one fact.
//
// The floors are checked in the order a reviewer fixes them: nodes, then the
// workers those nodes sit on, then the devices those workers hand over, then
// the failure domains they are spread across. A document that names no cluster
// at all, or one that grows a cluster which is not there, is answered by the
// checks that own those refusals; this one has nothing to say about a
// deployment whose shape is not yet known and says nothing.
func StripeChecks(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]StripeCheck, error) {
	subject, known, err := stripeSubjectOf(ctx, reader, namespace, config)
	if err != nil || !known {
		return nil, err
	}
	total := newFootprint()
	total.merge(subject.existing)
	total.merge(plannedFootprint(config, subject.slots))
	scheme := subject.scheme

	if !scheme.IsSupported() {
		// The minimum of a scheme that does not exist is not a fact about
		// anything, so it is not reported beside it.
		return []StripeCheck{{
			Reason: StripeUnsupported,
			Message: fmt.Sprintf(
				"%s is %s, which is not a scheme simplyblock supports (%s); the control "+
					"plane refuses the cluster it would be created with, and that refusal "+
					"lands after approval has made this document immutable",
				subject.name, scheme, erasurecoding.SupportedNotation()),
		}}, nil
	}

	if subject.topology.SingleNode {
		return singleNodeChecks(config, subject, total), nil
	}

	minimum := scheme.MinimumNodesFor(subject.topology)
	if total.nodes() < minimum {
		return []StripeCheck{{
			Reason: StripeBelowMinimumNodes,
			Message: fmt.Sprintf("%s is %s, which needs at least %d %s%s, and this document %s %d",
				subject.name, scheme, minimum, plural(minimum, "storage node", "storage nodes"),
				placementPhrase(scheme), leavesPhrase(config), total.nodes()),
		}}, nil
	}

	if total.hosts() < minimum {
		return []StripeCheck{{
			Reason: StripeBelowMinimumWorkers,
			Message: fmt.Sprintf(
				"%s is %s, which needs at least %d storage nodes, and the %d this "+
					"deployment has sit on %d %s; every node of a worker fails with the "+
					"worker, so the spare the scheme rebuilds onto is not a spare",
				subject.name, scheme, minimum, total.nodes(), total.hosts(),
				plural(total.hosts(), "worker", "workers")),
		}}, nil
	}

	if check := deviceChecks(config, subject); check != nil {
		return []StripeCheck{*check}, nil
	}
	if check := failureDomainCheck(config, subject); check != nil {
		return []StripeCheck{*check}, nil
	}
	return nil, nil
}

// singleNodeChecks holds a declared single-node cluster to what the control
// plane activates: exactly one storage node, and on it one device per chunk of
// a stripe (sbcli cluster_ops.activation_minimum_devices), counted from the
// groups that name their devices. A group that names none hands over every
// device the worker has, which only the node's own report can count.
func singleNodeChecks(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, subject stripeSubject, total footprint,
) []StripeCheck {
	if total.nodes() != 1 {
		return []StripeCheck{{
			Reason: StripeSingleNodeCount,
			Message: fmt.Sprintf(
				"the cluster is a single-node cluster (clusterMode Single), which runs exactly one "+
					"storage node, and this document %s %d", leavesPhrase(config), total.nodes()),
		}}
	}

	minimum := subject.scheme.MinimumDevicesPerNode(erasurecoding.Topology{SingleNode: true})
	journal := subject.journalDevices()
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			named := namedDevices(group)
			if named == 0 {
				continue
			}
			if data := named - journal; data < minimum {
				return []StripeCheck{{
					Reason: StripeBelowMinimumDevices,
					Message: fmt.Sprintf(
						"%s is %s on a single-node cluster, which places each chunk of a "+
							"stripe on its own device and so needs at least %d storage "+
							"devices, and group %s of node set %s hands over %d%s",
						subject.name, subject.scheme, minimum, group.Name, set.Name, data,
						journalPhrase(journal)),
				}}
			}
		}
	}
	return nil
}

// deviceChecks holds the deployment to the two device floors: every storage
// node hands over at least one data device, and the cluster has the scheme's
// MinimumDataDevices of them across its nodes.
//
// Devices are counted per worker, because that is how they are stated: a group
// names what every worker of it hands over, and a node the cluster already has
// names what its worker gave it. A dedicated journal device (enableJournalDevice)
// is one per storage node and carries no data, so it is taken off before either
// floor is applied.
//
// A group that names no devices hands over every device its workers have, which
// only the node's own report can count, and an existing node that names none
// and has not reported is the same unknown. Such a worker is held to neither
// floor, and the cluster total is not asserted while any worker is unknown: a
// floor reported short on a count known to be incomplete is a finding about the
// counting, not about the deployment.
func deviceChecks(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, subject stripeSubject,
) *StripeCheck {
	scheme := subject.scheme
	journal := subject.journalDevices()
	slots := int(subject.slots)
	data := map[string]int{}
	unknown := false

	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			planned := 0
			for _, worker := range group.Workers {
				// A worker the cluster already has a node on gets nothing from
				// this document (the expansion creates only the empty slots),
				// so its devices are the node's to count.
				if _, held := subject.existing.workers[worker]; !held {
					planned++
				}
			}
			if planned == 0 {
				continue
			}
			named := namedDevices(group)
			if named == 0 {
				unknown = true
				continue
			}
			perWorker := max(0, named-journal*slots)
			if perWorker < slots {
				return &StripeCheck{
					Reason: StripeBelowMinimumDevices,
					Message: fmt.Sprintf(
						"%s is %s, and every storage node needs at least one data device to "+
							"place a chunk on, but group %s of node set %s hands over %d%s%s (%s)",
						subject.name, scheme, group.Name, set.Name, perWorker,
						journalPhrase(journal), slotsPhrase(slots), nameWorkers(group.Workers)),
				}
			}
			for _, worker := range group.Workers {
				if _, held := subject.existing.workers[worker]; !held {
					data[worker] = perWorker
				}
			}
		}
	}

	for i := range subject.nodes {
		node := &subject.nodes[i]
		count, known := nodeDataDevices(node, journal)
		if !known {
			unknown = true
			continue
		}
		if count < 1 {
			return &StripeCheck{
				Reason: StripeBelowMinimumDevices,
				Message: fmt.Sprintf(
					"%s is %s, and every storage node needs at least one data device to "+
						"place a chunk on, but storage node %s on worker %s hands over none%s",
					subject.name, scheme, node.Name, node.Spec.WorkerNode, journalPhrase(journal)),
			}
		}
		data[node.Spec.WorkerNode] += count
	}

	if unknown {
		return nil
	}
	sum := 0
	for _, count := range data {
		sum += count
	}
	minimum := scheme.MinimumDataDevicesFor(subject.topology, subject.raidDevices)
	if sum >= minimum {
		return nil
	}
	return &StripeCheck{
		Reason: StripeBelowMinimumDevices,
		Message: fmt.Sprintf(
			"%s is %s, which needs at least %d data %s across the cluster%s, and this "+
				"document %s %d%s",
			subject.name, scheme, minimum, plural(minimum, "device", "devices"),
			devicePlacementPhrase(scheme, subject.raidDevices), leavesPhrase(config), sum,
			journalPhrase(journal)),
	}
}

// failureDomainCheck holds a deployment whose groups declare failure domains to
// the scheme's MinimumFailureDomains: one distinct domain per chunk of a stripe,
// so that losing a domain costs a stripe one chunk and never two. A document
// whose groups declare none, growing a cluster whose nodes declare none, is not
// spread over domains and is not held to it.
//
// The control plane has a rule of its own for a cluster created in
// failure-domain mode (enableFailureDomains): parity plus two domains, equal in
// size, or it refuses to activate, which FailureDomainLayout reports. Where that
// document's groups all declare a domain and the control plane's floor is at
// least the stripe's, the count is left to that finding rather than reported
// twice under two names for one shortage. Where the stripe's floor is the
// higher one, it is the number the reviewer has to reach, and it is reported here.
func failureDomainCheck(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, subject stripeSubject,
) *StripeCheck {
	domains := map[string]struct{}{}
	declared := true
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.FailureDomain == "" {
				declared = false
				continue
			}
			domains[group.FailureDomain] = struct{}{}
		}
	}
	minimum := subject.scheme.MinimumFailureDomains()
	if template := config.Spec.Cluster; config.Spec.ClusterRef == "" && template != nil &&
		ptr.BoolFromOrFalse(template.EnableFailureDomains) && declared &&
		subject.scheme.ParityChunks+2 >= minimum {
		return nil
	}
	for i := range subject.nodes {
		if domain := subject.nodes[i].Spec.Config.FailureDomain; domain != "" {
			domains[domain] = struct{}{}
		}
	}
	if len(domains) == 0 || len(domains) >= minimum {
		return nil
	}
	return &StripeCheck{
		Reason: StripeBelowMinimumFailureDomains,
		Message: fmt.Sprintf(
			"%s is %s, which places each chunk of a stripe in its own failure domain and "+
				"so needs at least %d distinct failure %s, and this document %s %d (%s)",
			subject.name, subject.scheme, minimum, plural(minimum, "domain", "domains"),
			leavesPhrase(config), len(domains), strings.Join(sortedKeys(domains), ", ")),
	}
}

// namedDevices is how many devices a group names, of whichever class.
func namedDevices(group simplyblockv1alpha2.NodeGroup) int {
	if group.Devices == nil {
		return 0
	}
	return len(group.Devices.NVMe) + len(group.Devices.Block)
}

// nodeDataDevices is how many data devices a storage node the cluster already
// has hands over, from the device names its configuration states less the one
// a journal takes, or failing those from the count the control plane reported
// for it, which lists storage devices only. known is false for a node that
// names none and has not been reported on.
func nodeDataDevices(node *simplyblockv1alpha2.StorageNode, journal int) (count int, known bool) {
	if names := node.Spec.Config.DeviceNames; len(names) > 0 {
		return max(0, len(names)-journal), true
	}
	if resources := node.Status.Resources; resources != nil && resources.Devices != nil {
		return int(resources.Devices.Total), true
	}
	return 0, false
}

func journalPhrase(journal int) string {
	if journal == 0 {
		return ""
	}
	return " after the one enableJournalDevice dedicates to the journal"
}

// slotsPhrase says what the per-worker count is measured against when a worker
// runs more than one storage node, each of which needs a device of its own.
func slotsPhrase(slots int) string {
	if slots <= 1 {
		return ""
	}
	return fmt.Sprintf(" to the %d storage nodes on each of its workers", slots)
}

// nameWorkers names a group's workers in a message without letting two hundred
// of them become the message.
func nameWorkers(workers []string) string {
	const shown = 3
	if len(workers) <= shown {
		return strings.Join(workers, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(workers[:shown], ", "), len(workers)-shown)
}

// placementPhrase accounts for the minimum, because a number a reviewer cannot
// account for is a number they cannot act on: it is the stripe's own width plus
// a spare for each failure the stripe survives.
func placementPhrase(scheme erasurecoding.Scheme) string {
	if scheme.ParityChunks == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d to place a stripe across and %d %s to rebuild onto)",
		scheme.DataChunks+scheme.ParityChunks, scheme.ParityChunks,
		plural(scheme.ParityChunks, "spare", "spares"))
}

// devicePlacementPhrase is placementPhrase for the device floor, which
// raidDevices lowers to the stripe's width because a disk failure under a
// RAID-protected device costs the stripe nothing to rebuild.
func devicePlacementPhrase(scheme erasurecoding.Scheme, raidDevices bool) string {
	if scheme.ParityChunks == 0 {
		return ""
	}
	if raidDevices {
		return " (one per chunk of a stripe; raidDevices declares every device protected " +
			"below simplyblock, so no spare device is kept for a rebuild)"
	}
	return fmt.Sprintf(" (%d to place a stripe across and %d spare %s to rebuild onto)",
		scheme.DataChunks+scheme.ParityChunks, scheme.ParityChunks,
		plural(scheme.ParityChunks, "device", "devices"))
}

// leavesPhrase says whether the count that follows is what the document builds
// or what it leaves behind, which are different things for a growth document.
func leavesPhrase(config *simplyblockv1alpha2.ClusterDeploymentConfig) string {
	if config.Spec.ClusterRef != "" {
		return "leaves the cluster with"
	}
	return "produces"
}

// stripeSubject is what a document's erasure coding is answered against: the
// scheme, what to call it in a message, the cluster's declarations that bear on
// the floors, how many storage nodes each of its workers runs, and what the
// cluster already has.
type stripeSubject struct {
	scheme erasurecoding.Scheme
	name   string
	// topology is what the cluster declares (spec.cluster.clusterMode, or the
	// referenced cluster's spec.clusterMode).
	topology erasurecoding.Topology
	// raidDevices is the cluster's declaration that every data device is
	// protected below simplyblock, which lowers the device floor to the
	// stripe's width.
	raidDevices bool
	// journalDevice is enableJournalDevice: every storage node dedicates one of
	// its devices to the journal, and that device carries no data.
	journalDevice bool
	// slots is how many storage nodes each worker runs.
	slots int32
	// existing is what the cluster already has on this site, and nodes are
	// those same storage nodes, for their devices and failure domains.
	existing footprint
	nodes    []simplyblockv1alpha2.StorageNode
}

// journalDevices is how many devices each storage node gives to its journal.
func (s stripeSubject) journalDevices() int {
	if s.journalDevice {
		return 1
	}
	return 0
}

// stripeSubjectOf resolves the subject of a document. known is false for a
// document whose cluster cannot be resolved, which is a refusal of its own
// elsewhere.
func stripeSubjectOf(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (subject stripeSubject, known bool, err error) {
	if config.Spec.ClusterRef == "" {
		template := config.Spec.Cluster
		if template == nil {
			return subject, false, nil
		}
		return stripeSubject{
			scheme:        erasurecoding.SchemeOf(template.Stripe),
			name:          "spec.cluster.stripe",
			topology:      erasurecoding.TopologyOf(template.ClusterMode),
			raidDevices:   ptr.BoolFromOrFalse(template.RaidDevices),
			journalDevice: ptr.BoolFromOrFalse(template.EnableJournalDevice),
			slots:         slotsOf(template.SocketsToUse, template.NodesPerSocket),
			existing:      newFootprint(),
		}, true, nil
	}

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: namespace, Name: config.Spec.ClusterRef}
	switch err := reader.Get(ctx, key, &cluster); {
	case apierrors.IsNotFound(err):
		return subject, false, nil
	case err != nil:
		return subject, false, fmt.Errorf("reading StorageCluster %s: %w", config.Spec.ClusterRef, err)
	}

	held, nodes, err := existingFootprint(ctx, reader, namespace, cluster.Name)
	if err != nil {
		return subject, false, err
	}
	return stripeSubject{
		scheme:      erasurecoding.SchemeOf(cluster.Spec.Stripe),
		name:        fmt.Sprintf("the stripe of cluster %s", cluster.Name),
		topology:    erasurecoding.TopologyOf(cluster.Spec.ClusterMode),
		raidDevices: ptr.BoolFromOrFalse(cluster.Spec.RaidDevices),
		journalDevice: cluster.Spec.StorageNodes != nil &&
			ptr.BoolFromOrFalse(cluster.Spec.StorageNodes.EnableJournalDevice),
		slots:    slotsPerWorker(&cluster),
		existing: held,
		nodes:    nodes,
	}, true, nil
}

// plannedFootprint is every storage node the document would produce, one per
// slot per worker.
func plannedFootprint(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, slots int32,
) footprint {
	planned := newFootprint()
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				for slot := int32(0); slot < slots; slot++ {
					planned.add(worker, slot)
				}
			}
		}
	}
	return planned
}

// existingFootprint is every storage node a cluster already has on this site,
// and the nodes themselves. A remote node is the peer site's node of a stretched
// cluster and is skipped.
func existingFootprint(
	ctx context.Context, reader client.Reader, namespace, cluster string,
) (footprint, []simplyblockv1alpha2.StorageNode, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := reader.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return footprint{}, nil, fmt.Errorf("listing the nodes of cluster %s: %w", cluster, err)
	}
	held := newFootprint()
	var own []simplyblockv1alpha2.StorageNode
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Spec.ClusterRef != cluster || node.IsRemote() {
			continue
		}
		slot := int32(0)
		if node.Spec.Slot != nil {
			slot = *node.Spec.Slot
		}
		held.add(node.Spec.WorkerNode, slot)
		own = append(own, *node)
	}
	return held, own, nil
}

// ClusterStripeChecks is StripeChecks for every cluster the document builds:
// the document as a whole for one cluster, and each node set of a
// supercluster's document as the member it becomes, since a member stripes
// across its own nodes alone (design-superclusters.md §5.4). A finding about a
// member names its node set. Admission and the controller's validation both
// read it, so the two cannot disagree about which document is short.
func ClusterStripeChecks(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]StripeCheck, error) {
	members, err := AddsMembers(ctx, reader, config)
	if err != nil {
		return nil, err
	}
	if members {
		config = asCreation(config)
	}
	var checks []StripeCheck
	for _, scope := range clusterScopes(config, members) {
		found, err := StripeChecks(ctx, reader, namespace, scope.document)
		if err != nil {
			return nil, err
		}
		for _, check := range found {
			check.Message = scope.prefix + check.Message
			checks = append(checks, check)
		}
	}
	return checks, nil
}
