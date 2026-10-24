// The failure-domain layout the control plane activates a cluster with, mirrored
// from sbcli's activation gate (cluster_ops.cluster_activate and
// planner.fd_activation_domain_count_violation).
//
// The control plane checks it only at activation, after the cluster has been
// created and every node added, and refuses outright. Discovery asks it before
// proposing failure-domain mode, and validation asks it before approval, so the
// refusal lands while the document can still be edited.

package erasurecoding

import (
	"fmt"
	"sort"
	"strings"
)

// FailureDomainViolation answers a layout, the hosts in each failure domain,
// against what a fresh activation accepts: at least ParityChunks+2 distinct
// domains, so a single add or remove after a domain's loss still has a spare
// domain to relocate onto, and the same number of hosts in every domain. It
// returns the reason the layout is refused, or an empty string for one that is
// accepted.
func FailureDomainViolation(scheme Scheme, hostsPerDomain map[string]int) string {
	minimum := scheme.ParityChunks + 2
	if len(hostsPerDomain) < minimum {
		return fmt.Sprintf(
			"failure-domain mode with %s needs at least %d distinct failure domains (the stripe's "+
				"parity plus two), and the workers are in %d (%s)",
			scheme, minimum, len(hostsPerDomain), describeLayout(hostsPerDomain))
	}
	sizes := map[int]struct{}{}
	for _, hosts := range hostsPerDomain {
		sizes[hosts] = struct{}{}
	}
	if len(sizes) > 1 {
		return fmt.Sprintf(
			"failure-domain mode needs an equal number of workers in every failure domain when "+
				"the cluster is activated, and the domains hold %s", describeLayout(hostsPerDomain))
	}
	return ""
}

// describeLayout renders the layout in domain order, as in a: 2, b: 1.
func describeLayout(hostsPerDomain map[string]int) string {
	domains := make([]string, 0, len(hostsPerDomain))
	for domain := range hostsPerDomain {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	parts := make([]string, 0, len(domains))
	for _, domain := range domains {
		parts = append(parts, fmt.Sprintf("%s: %d", domain, hostsPerDomain[domain]))
	}
	return strings.Join(parts, ", ")
}
