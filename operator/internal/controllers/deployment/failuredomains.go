// The failure-domain mapping an expansion writes onto its cluster. The document
// names a fault group with a label ("rack-b") and the control plane identifies
// one by an integer, so CreatingNodes records an index for every label before it
// creates a node that declares one.

package deployment

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// maxFailureDomains is how many entries StorageCluster.status.failureDomains
// holds, which is the MaxItems marker on that field.
const maxFailureDomains = 256

// assignFailureDomains returns the mapping with an index for every label in
// labels, ordered by index.
//
// An assigned entry never changes: the control plane has placed nodes, and so
// data, under that index. New labels are given indices in two passes. A label
// that is a number first claims its own value, because before the mapping
// existed a numeric label was sent as that number, and a cluster built then has
// nodes under it. Every other label, in name order, then takes the lowest index
// still free.
func assignFailureDomains(
	assigned []simplyblockv1alpha2.FailureDomainIndex, labels []string,
) []simplyblockv1alpha2.FailureDomainIndex {
	mapping := slices.Clone(assigned)
	known := map[string]bool{}
	taken := map[int32]bool{}
	for _, entry := range mapping {
		known[entry.Name] = true
		taken[entry.Index] = true
	}

	var fresh []string
	for _, label := range labels {
		if label != "" && !known[label] {
			known[label] = true
			fresh = append(fresh, label)
		}
	}
	sort.Strings(fresh)

	var named []string
	for _, label := range fresh {
		number, err := strconv.ParseInt(label, 10, 32)
		if err != nil || number < 0 || taken[int32(number)] {
			named = append(named, label)
			continue
		}
		taken[int32(number)] = true
		mapping = append(mapping, simplyblockv1alpha2.FailureDomainIndex{
			Name: label, Index: int32(number),
		})
	}

	next := int32(0)
	for _, label := range named {
		for taken[next] {
			next++
		}
		taken[next] = true
		mapping = append(mapping, simplyblockv1alpha2.FailureDomainIndex{
			Name: label, Index: next,
		})
	}

	sort.SliceStable(mapping, func(i, j int) bool { return mapping[i].Index < mapping[j].Index })
	return mapping
}

// failureDomainsOf lists every failure-domain label the document's groups declare.
func failureDomainsOf(config *simplyblockv1alpha2.ClusterDeploymentConfig) []string {
	var labels []string
	for _, set := range config.Spec.NodeSets {
		for _, group := range set.Groups {
			if group.FailureDomain != "" {
				labels = append(labels, group.FailureDomain)
			}
		}
	}
	return labels
}

// mapFailureDomains writes the document's labels into the cluster's
// status.failureDomains and leaves the cluster as written.
//
// The patch carries an optimistic lock because the list is replaced whole: the
// StorageCluster controller writes the same status, and a merge computed from a
// stale read would drop an entry another document added in between.
func (r *ClusterDeploymentConfigReconciler) mapFailureDomains(
	ctx context.Context,
	config *simplyblockv1alpha2.ClusterDeploymentConfig,
	cluster *simplyblockv1alpha2.StorageCluster,
) error {
	labels := failureDomainsOf(config)
	if len(labels) == 0 {
		return nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh simplyblockv1alpha2.StorageCluster
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), &fresh); err != nil {
			return err
		}
		mapping := assignFailureDomains(fresh.Status.FailureDomains, labels)
		if equality.Semantic.DeepEqual(fresh.Status.FailureDomains, mapping) {
			*cluster = fresh
			return nil
		}
		patch := client.MergeFromWithOptions(fresh.DeepCopy(),
			client.MergeFromWithOptimisticLock{})
		fresh.Status.FailureDomains = mapping
		if err := r.Status().Patch(ctx, &fresh, patch); err != nil {
			return err
		}
		*cluster = fresh
		return nil
	})
	if err != nil {
		return fmt.Errorf("recording the failure domains of StorageCluster %s: %w",
			cluster.Name, err)
	}
	return nil
}
