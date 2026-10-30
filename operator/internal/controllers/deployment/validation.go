// What a draft is checked against, and why the check runs on every pass.
//
// This is the only check a device gets before the deployment runs, because
// admission does not look at devices (§5.1). A config discovery wrote cannot be
// wrong about them, since the list came from the inspection; a hand-written one
// can, and saying so while the document is still editable is the whole value of
// the review gate. Approving without reading it is how a device mistake becomes an
// immutable document, and no mechanism below the reviewer prevents that.
//
// Every finding is a statement about the world rather than about the document's
// structure. The structural rules — one device class per document, a selection
// naming one member, immutability after approval — are CEL on the type, because
// they hold for a draft as firmly as for an approval and a draft that breaks one
// should not be storable at all.
//
// design-clusterdeploymentconfig.md §4.1 and §9.1 are the specification.

package deployment

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/erasurecoding"
)

// finding is one thing validation found, carrying the reason its event is raised
// under so the two cannot drift.
type finding struct {
	reason  string
	message string
}

// validate reads the document against the cluster it would act on and returns
// everything wrong with it.
//
// It returns every finding rather than the first, because a reviewer fixing a
// document wants the whole list: a draft that names three missing workers should
// say so once rather than over three edits.
func (r *ClusterDeploymentConfigReconciler) validate(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]finding, error) {
	var findings []finding

	// A document adding members to a supercluster creates each of them, so it is
	// checked as a document creating them, at the sizing it states
	// (design-superclusters.md §5.6).
	members, err := AddsMembers(ctx, r.Client, config)
	if err != nil {
		return nil, err
	}
	if members && config.Spec.ClusterRef != "" {
		if config.Spec.Cluster == nil {
			findings = append(findings, finding{reason: MemberSizingUnstated, message: fmt.Sprintf(
				"spec.clusterRef names supercluster %s, and the members the document adds have no "+
					"sizing: state it in spec.cluster", config.Spec.ClusterRef)})
		}
		config = asCreation(config)
	}

	missing, err := r.missingWorkers(ctx, config)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		findings = append(findings, finding{
			reason: WorkerNotFound,
			message: fmt.Sprintf("%s %s not a node of this Kubernetes cluster",
				strings.Join(missing, ", "), plural(len(missing), "is", "are")),
		})
	}

	if found := r.groupsWithoutDevices(config); len(found) > 0 {
		findings = append(findings, finding{
			reason: DeviceNotFound,
			message: fmt.Sprintf("group %s %s no devices, so its workers hand nothing to simplyblock",
				strings.Join(found, ", "), plural(len(found), "names", "name")),
		})
	}

	mismatch, err := r.deviceClassMismatch(ctx, config)
	if err != nil {
		return nil, err
	}
	if mismatch != "" {
		findings = append(findings, finding{reason: DeviceClassMismatch, message: mismatch})
	}

	if found := r.missingFailureDomains(ctx, config); found != "" {
		findings = append(findings, finding{reason: DeviceNotFound, message: found})
	}

	overflow, err := r.failureDomainOverflow(ctx, config)
	if err != nil {
		return nil, err
	}
	if overflow != "" {
		findings = append(findings, finding{reason: TooManyFailureDomains, message: overflow})
	}

	if found := duplicateWorkers(config); len(found) > 0 {
		findings = append(findings, finding{
			reason: WorkerNotFound,
			message: fmt.Sprintf(
				"%s %s in more than one group, and a storage node is identified by its "+
					"worker and slot alone, so only the first group's devices and settings "+
					"would reach it",
				strings.Join(found, ", "), plural(len(found), "appears", "appear")),
		})
	}

	// What spans one cluster spans one node set of a supercluster's document,
	// since each node set becomes a member cluster (design-superclusters.md
	// §5.1): its stripe, its interfaces, and its limits.
	stripe, err := ClusterStripeChecks(ctx, r.Client, config.Namespace, config)
	if err != nil {
		return nil, err
	}
	for _, check := range stripe {
		findings = append(findings, finding{reason: check.Reason, message: check.Message})
	}
	for _, scope := range clusterScopes(config, members) {
		if found := conflictingInterfaces(scope.document); found != "" {
			findings = append(findings, finding{reason: WorkerNotFound, message: scope.prefix + found})
		}
		if found := failureDomainLayoutRefusal(scope.document); found != "" {
			findings = append(findings, finding{reason: FailureDomainLayout, message: scope.prefix + found})
		}
	}
	findings = append(findings, memberLimits(config, members)...)
	topology, err := r.topologyFindings(ctx, config)
	if err != nil {
		return nil, err
	}
	findings = append(findings, topology...)
	grown, err := r.growthLimit(ctx, config, members)
	if err != nil {
		return nil, err
	}
	if grown != "" {
		findings = append(findings, finding{reason: MemberTooLarge, message: grown})
	}
	if groups := groupsWithNetworkAttachments(config); len(groups) > 0 {
		findings = append(findings, finding{
			reason: NetworkAttachmentNotDeployable,
			message: fmt.Sprintf(
				"group(s) %s attach their storage nodes through NetworkAttachmentDefinitions, "+
					"and this operator does not build storage nodes on network attachments yet",
				strings.Join(groups, ", ")),
		})
	}
	if groups := groupsWithoutManagementInterface(config); len(groups) > 0 && !onANetwork(config) {
		findings = append(findings, finding{
			reason: NoManagementInterface,
			message: fmt.Sprintf(
				"group(s) %s name no management interface, and the control plane refuses a "+
					"node it cannot find a management address on",
				strings.Join(groups, ", ")),
		})
	}
	return findings, nil
}

// groupsWithoutManagementInterface names the groups that would be expanded into
// nodes the control plane refuses.
//
// It is worth a finding of its own because of where the refusal otherwise lands.
// The control plane does not reject the request; it accepts it, starts a node_add
// task, and fails inside it. What an administrator sees is a task that gave up on
// a document that looked complete, several steps and one approval away from the
// field that was empty.
func groupsWithoutManagementInterface(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) []string {
	var missing []string
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.MgmtInterface == "" && group.NetworkAttachment == nil {
				missing = append(missing, group.Name)
			}
		}
	}
	return missing
}

// duplicateWorkers names every worker the document lists in more than one group.
//
// The schema permits it and the expansion cannot honor it: a StorageNode is
// identified by its cluster, its worker, and its slot, so the first group to reach
// a worker creates its nodes and every later group's device list, fault group, and
// memory setting is discarded by the create that finds one already there. Which
// group wins is the document's order, which is not a thing anybody chose.
func duplicateWorkers(config *simplyblockv1alpha2.ClusterDeploymentConfig) []string {
	seen := map[string]int{}
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				seen[worker]++
			}
		}
	}

	repeated := map[string]struct{}{}
	for worker, count := range seen {
		if count > 1 {
			repeated[worker] = struct{}{}
		}
	}
	return sortedKeys(repeated)
}

// conflictingInterfaces reports groups that name different network interfaces.
//
// A DaemonSet is one object for every node it schedules and its pod template
// cannot differ per group, so the interfaces are the cluster's whichever group
// states them. A document whose groups disagree therefore describes something the
// expansion cannot build, and taking the first silently would bind every node to
// one group's network while the document said otherwise.
func conflictingInterfaces(config *simplyblockv1alpha2.ClusterDeploymentConfig) string {
	mgmt := map[string]struct{}{}
	data := map[string]struct{}{}
	attachments := map[string]struct{}{}
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if attachment := group.NetworkAttachment; attachment != nil {
				attachments[attachmentLabel(attachment)] = struct{}{}
			}
			if group.MgmtInterface != "" {
				mgmt[group.MgmtInterface] = struct{}{}
			}
			if len(group.DataInterfaces) > 0 {
				data[strings.Join(group.DataInterfaces, ",")] = struct{}{}
			}
		}
	}

	if len(mgmt) > 1 {
		return fmt.Sprintf(
			"the groups name different management interfaces (%s), and one DaemonSet "+
				"serves every node of a cluster, so they cannot differ",
			strings.Join(sortedKeys(mgmt), ", "))
	}
	if len(data) > 1 {
		return fmt.Sprintf(
			"the groups name different data interfaces (%s), and one DaemonSet serves "+
				"every node of a cluster, so they cannot differ",
			strings.Join(sortedKeys(data), "; "))
	}
	if len(attachments) > 1 {
		return fmt.Sprintf(
			"the groups name different network attachments (%s), and one DaemonSet serves "+
				"every node of a cluster, so they cannot differ",
			strings.Join(sortedKeys(attachments), "; "))
	}
	return ""
}

// attachmentLabel is a group's attachment as a reader compares it.
func attachmentLabel(attachment *simplyblockv1alpha2.NetworkAttachmentSelection) string {
	if attachment.Data == "" {
		return "management " + attachment.Management
	}
	return "management " + attachment.Management + ", data " + attachment.Data
}

// missingWorkers names every worker the document lists that is not a node of this
// Kubernetes cluster.
func (r *ClusterDeploymentConfigReconciler) missingWorkers(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]string, error) {
	var workers corev1.NodeList
	if err := r.List(ctx, &workers); err != nil {
		return nil, fmt.Errorf("listing the Kubernetes workers: %w", err)
	}
	present := make(map[string]struct{}, len(workers.Items))
	for i := range workers.Items {
		present[workers.Items[i].Name] = struct{}{}
	}

	missing := map[string]struct{}{}
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				if _, there := present[worker]; !there {
					missing[worker] = struct{}{}
				}
			}
		}
	}
	return sortedKeys(missing), nil
}

// groupsWithoutDevices names every group that hands no devices over.
//
// The schema permits it, because a selection is optional and a group may
// legitimately be written before its devices are known. What it cannot be is
// approved that way: a node with no devices comes up carrying nothing.
func (r *ClusterDeploymentConfigReconciler) groupsWithoutDevices(
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
) []string {
	var found []string
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if len(devicesOf(group)) == 0 {
				found = append(found, set.Name+"/"+group.Name)
			}
		}
	}
	return found
}

// deviceClassMismatch reports a growth document whose groups name devices of a
// class its cluster is not built out of.
//
// It applies only to a document that names an existing cluster. For one that
// creates its own, the class is whatever the groups say and there is nothing to
// disagree with — which is why the expansion stamps it rather than checking it.
func (r *ClusterDeploymentConfigReconciler) deviceClassMismatch(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (string, error) {
	if config.Spec.ClusterRef == "" {
		return "", nil
	}

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: config.Namespace, Name: config.Spec.ClusterRef}
	switch err := r.Get(ctx, key, &cluster); {
	case apierrors.IsNotFound(err):
		// The expansion refuses this with ClusterNotFound, and saying it twice
		// would put two findings on one fact.
		return "", nil
	case err != nil:
		return "", fmt.Errorf("reading StorageCluster %s: %w", config.Spec.ClusterRef, err)
	}

	stated := DeviceClassOf(config)
	if stated == "" {
		return "", nil
	}

	held := cluster.Spec.DeviceClass
	if held == "" {
		// An unstated class is NVMe, which is what the cluster's own field
		// defaults to and what describes every cluster predating the field.
		held = simplyblockv1alpha2.StorageClusterDeviceClassNVMe
	}
	if stated == held {
		return "", nil
	}
	return fmt.Sprintf(
		"the groups name %s devices and cluster %s is built out of %s; "+
			"an erasure-coding stripe placed across both classes is written at the slower one's rate",
		stated, cluster.Name, held), nil
}

// missingFailureDomains reports a document whose cluster requires fault groups and
// whose groups do not all declare one.
//
// Provisioning holds on this rather than failing, so a document approved without
// them produces nodes that sit and wait. Saying so at draft time is what turns
// that into an edit rather than an investigation.
func (r *ClusterDeploymentConfigReconciler) missingFailureDomains(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) string {
	if !r.failureDomainsRequired(ctx, config) {
		return ""
	}

	var found []string
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.FailureDomain == "" {
				found = append(found, set.Name+"/"+group.Name)
			}
		}
	}
	if len(found) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"the cluster has failure domains enabled and group %s %s none; "+
			"provisioning holds until each declares one",
		strings.Join(found, ", "), plural(len(found), "declares", "declare"))
}

// failureDomainOverflow reports a document whose failure-domain labels would
// grow the cluster's mapping past what status.failureDomains holds.
//
// The mapping only grows, so a growth document is counted against what its
// cluster has already mapped. Past the limit the status patch in CreatingNodes is
// refused by the apiserver on every pass, and the expansion would stall after
// approval rather than say so in the draft.
func (r *ClusterDeploymentConfigReconciler) failureDomainOverflow(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) (string, error) {
	var assigned []simplyblockv1alpha2.FailureDomainIndex
	if config.Spec.ClusterRef != "" {
		var cluster simplyblockv1alpha2.StorageCluster
		key := client.ObjectKey{Namespace: config.Namespace, Name: config.Spec.ClusterRef}
		err := r.Get(ctx, key, &cluster)
		switch {
		case apierrors.IsNotFound(err):
			// ClusterNotFound is reported by the step that resolves the cluster.
		case err != nil:
			return "", fmt.Errorf("reading StorageCluster %s: %w", config.Spec.ClusterRef, err)
		default:
			assigned = cluster.Status.FailureDomains
		}
	}

	total := len(assignFailureDomains(assigned, failureDomainsOf(config)))
	if total <= maxFailureDomains {
		return "", nil
	}
	return fmt.Sprintf(
		"the document would bring the cluster to %d failure domains, and a cluster maps "+
			"at most %d", total, maxFailureDomains), nil
}

// failureDomainsRequired reads the flag from whichever cluster the document acts
// on: the template for one it creates, the live object for one it grows.
func (r *ClusterDeploymentConfigReconciler) failureDomainsRequired(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) bool {
	if config.Spec.ClusterRef == "" {
		template := config.Spec.Cluster
		return template != nil && template.EnableFailureDomains != nil &&
			*template.EnableFailureDomains
	}

	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: config.Namespace, Name: config.Spec.ClusterRef}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return false
	}
	return cluster.Spec.EnableFailureDomains != nil && *cluster.Spec.EnableFailureDomains
}

// failureDomainLayoutRefusal is why the control plane would refuse to activate
// the cluster a document creates in failure-domain mode, or nothing. The control
// plane checks the layout at a fresh activation alone, after the cluster has
// been created and every node added, so a growth document is not held to it.
func failureDomainLayoutRefusal(config *simplyblockv1alpha2.ClusterDeploymentConfig) string {
	template := config.Spec.Cluster
	if config.Spec.ClusterRef != "" || template == nil || !ptr.BoolFromOrFalse(template.EnableFailureDomains) {
		return ""
	}
	hosts := map[string]map[string]struct{}{}
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.FailureDomain == "" {
				// Reported by missingFailureDomains; the layout is not known.
				return ""
			}
			if hosts[group.FailureDomain] == nil {
				hosts[group.FailureDomain] = map[string]struct{}{}
			}
			for _, worker := range group.Workers {
				hosts[group.FailureDomain][worker] = struct{}{}
			}
		}
	}
	layout := make(map[string]int, len(hosts))
	for domain, workers := range hosts {
		layout[domain] = len(workers)
	}
	return erasurecoding.FailureDomainViolation(erasurecoding.SchemeOf(template.Stripe), layout)
}

// summarize renders the findings for status.message: one sentence, which is what
// the field is for, with the rest countable.
func summarize(findings []finding) string {
	if len(findings) == 0 {
		return ""
	}
	if len(findings) == 1 {
		return findings[0].message
	}
	return fmt.Sprintf("%s (and %d other %s)", findings[0].message,
		len(findings)-1, plural(len(findings)-1, "problem", "problems"))
}

// sortedKeys renders a set as a stable list, so a message does not reorder itself
// between passes and churn the status.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// plural picks the word for a count, so a message reads as a sentence.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// clusterScope is the part of a document one cluster is built from, and the
// words a finding about it opens with.
type clusterScope struct {
	document *simplyblockv1alpha2.ClusterDeploymentConfig
	prefix   string
}

// clusterScopes are the clusters a document builds: the whole document for one
// cluster, and one node set per member for a members document.
func clusterScopes(config *simplyblockv1alpha2.ClusterDeploymentConfig, members bool) []clusterScope {
	if !members {
		return []clusterScope{{document: config}}
	}
	scopes := make([]clusterScope, 0, len(config.Spec.NodeSets))
	for _, set := range config.Spec.NodeSets {
		scopes = append(scopes, clusterScope{
			document: memberView(config, set),
			prefix:   fmt.Sprintf("node set %s: ", set.Name),
		})
	}
	return scopes
}

// maxStorageNodesPerCluster bounds a cluster's exposure to concurrent failures:
// its fault tolerance is its stripe's and does not grow with its node count,
// while the chance that more nodes fail at once than the stripe tolerates grows
// with every node (design-superclusters.md §5.2).
const maxStorageNodesPerCluster = 32

// memberLimits reports node sets over the storage-node limit, and the members a
// supercluster's document would name with something that is not a name.
func memberLimits(config *simplyblockv1alpha2.ClusterDeploymentConfig, members bool) []finding {
	var findings []finding
	for _, set := range config.Spec.NodeSets {
		// The node set's own layout, its overrides merged into the template.
		slots := int32(1)
		if template := memberView(config, set).Spec.Cluster; template != nil {
			slots = slotsOf(template.SocketsToUse, template.NodesPerSocket)
		}
		workers := 0
		for _, group := range set.Groups {
			workers += len(group.Workers)
		}
		if nodes := workers * int(slots); nodes > maxStorageNodesPerCluster {
			findings = append(findings, finding{reason: MemberTooLarge, message: fmt.Sprintf(
				"node set %s makes %d storage nodes, and a cluster holds at most %d: split its "+
					"workers into node sets of at most %d workers, each a member of a supercluster",
				set.Name, nodes, maxStorageNodesPerCluster, maxStorageNodesPerCluster/int(slots))})
		}
		if !members {
			continue
		}
		name := memberName(superclusterName(config), set.Name, "abc")
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			findings = append(findings, finding{reason: MemberNameInvalid, message: fmt.Sprintf(
				"node set %s makes a member named like %s, which a StorageCluster cannot be "+
					"named (%s): shorten the node set's name or the cluster's",
				set.Name, name, strings.Join(errs, "; "))})
		}
	}
	return findings
}

// growthLimit reports a document that grows a cluster past the storage-node
// limit, counting the nodes the cluster already has. A document adding members
// is held to the limit per member by memberLimits instead.
func (r *ClusterDeploymentConfigReconciler) growthLimit(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig, members bool,
) (string, error) {
	if members || config.Spec.ClusterRef == "" {
		return "", nil
	}
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Namespace: config.Namespace, Name: config.Spec.ClusterRef}
	switch err := r.Get(ctx, key, &cluster); {
	case apierrors.IsNotFound(err):
		// The expansion refuses this with ClusterNotFound.
		return "", nil
	case err != nil:
		return "", fmt.Errorf("reading StorageCluster %s: %w", config.Spec.ClusterRef, err)
	}
	held, err := existingFootprint(ctx, r.Client, config.Namespace, cluster.Name)
	if err != nil {
		return "", err
	}
	total := newFootprint()
	total.merge(held)
	total.merge(plannedFootprint(config, slotsPerWorker(&cluster)))
	if total.nodes() <= maxStorageNodesPerCluster {
		return "", nil
	}
	return fmt.Sprintf(
		"the document adds %d storage nodes to cluster %s, which has %d, and a cluster holds at most %d",
		total.nodes()-held.nodes(), cluster.Name, held.nodes(), maxStorageNodesPerCluster), nil
}

// groupsWithNetworkAttachments names the groups that attach their storage nodes
// through NetworkAttachmentDefinitions.
//
// The document states the attachment and the storage nodes are not yet built
// with it: they run on host networking and would bind no management interface,
// which the control plane refuses inside node_add. Holding the document here
// says so before anything is created.
func groupsWithNetworkAttachments(config *simplyblockv1alpha2.ClusterDeploymentConfig) []string {
	var attached []string
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.NetworkAttachment != nil {
				attached = append(attached, group.Name)
			}
		}
	}
	return attached
}
