// The topology rules approval holds every document to (design-superclusters.md
// §5.7), read off the Kubernetes topology labels of the workers each node set
// lists. A member stays within one region, because it stripes every write
// across its own nodes and a member spanning regions puts inter-region latency
// in every write. With failure domains enabled, every member of a region holds
// every domain the region has, so a domain's loss takes nodes from every member
// and all of no member. Both hold for a document written by hand as much as for
// a draft, so they live with validation rather than with the generator.

package deployment

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// topologyFindings are the region and failure-domain findings of a document.
func (r *ClusterDeploymentConfigReconciler) topologyFindings(
	ctx context.Context, config *simplyblockv1alpha2.ClusterDeploymentConfig,
) ([]finding, error) {
	regions, err := r.workerRegions(ctx)
	if err != nil {
		return nil, err
	}

	var findings []finding
	setRegion := make(map[string]string, len(config.Spec.NodeSets))
	for _, set := range config.Spec.NodeSets {
		held := map[string]struct{}{}
		for _, group := range set.Groups {
			for _, worker := range group.Workers {
				// A worker without the label matches any region.
				if region := regions[worker]; region != "" {
					held[region] = struct{}{}
				}
			}
		}
		if len(held) > 1 {
			findings = append(findings, finding{reason: MemberSpansRegions, message: fmt.Sprintf(
				"node set %s lists workers in regions %s, and a member stays within one region",
				set.Name, strings.Join(sortedKeys(held), ", "))})
			continue
		}
		for region := range held {
			setRegion[set.Name] = region
		}
	}

	if template := config.Spec.Cluster; template != nil && template.EnableFailureDomains != nil &&
		*template.EnableFailureDomains {
		findings = append(findings, missingRegionDomains(config, setRegion)...)
	}
	return findings, nil
}

// workerRegions maps every worker carrying the region label to its region.
func (r *ClusterDeploymentConfigReconciler) workerRegions(ctx context.Context) (map[string]string, error) {
	var workers corev1.NodeList
	if err := r.List(ctx, &workers); err != nil {
		return nil, fmt.Errorf("listing the Kubernetes workers: %w", err)
	}
	regions := make(map[string]string, len(workers.Items))
	for i := range workers.Items {
		if region := workers.Items[i].Labels[corev1.LabelTopologyRegion]; region != "" {
			regions[workers.Items[i].Name] = region
		}
	}
	return regions, nil
}

// missingRegionDomains reports every node set lacking a failure domain another
// node set of its region holds. A node set whose workers carry no region label
// is in the unnamed region with the others that carry none.
func missingRegionDomains(
	config *simplyblockv1alpha2.ClusterDeploymentConfig, setRegion map[string]string,
) []finding {
	setDomains := map[string]map[string]struct{}{}
	regionDomains := map[string]map[string]struct{}{}
	for _, set := range config.Spec.NodeSets {
		domains := map[string]struct{}{}
		for _, group := range set.Groups {
			if group.FailureDomain != "" {
				domains[group.FailureDomain] = struct{}{}
			}
		}
		setDomains[set.Name] = domains
		region := setRegion[set.Name]
		if regionDomains[region] == nil {
			regionDomains[region] = map[string]struct{}{}
		}
		for domain := range domains {
			regionDomains[region][domain] = struct{}{}
		}
	}

	var findings []finding
	for _, set := range config.Spec.NodeSets {
		missing := map[string]struct{}{}
		for domain := range regionDomains[setRegion[set.Name]] {
			if _, held := setDomains[set.Name][domain]; !held {
				missing[domain] = struct{}{}
			}
		}
		if len(missing) == 0 {
			continue
		}
		findings = append(findings, finding{reason: MemberMissingFailureDomain, message: fmt.Sprintf(
			"node set %s holds no worker in failure domain %s, which another node set of its region "+
				"holds: every member of a region holds every failure domain the region has",
			set.Name, strings.Join(sortedKeys(missing), ", "))})
	}
	return findings
}
