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
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
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

	stripe, err := StripeChecks(ctx, r.Client, config.Namespace, config)
	if err != nil {
		return nil, err
	}
	for _, check := range stripe {
		findings = append(findings, finding{reason: check.Reason, message: check.Message})
	}

	if found := conflictingInterfaces(config); found != "" {
		findings = append(findings, finding{reason: WorkerNotFound, message: found})
	}
	if groups := groupsWithoutManagementInterface(config); len(groups) > 0 {
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
			if group.MgmtInterface == "" {
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
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
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
	return ""
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
