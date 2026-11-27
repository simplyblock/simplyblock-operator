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

// MinimumFailureDomains is how many distinct failure domains a fresh activation
// needs for the scheme, mirrored from planner.fd_activation_min_domains: the
// higher of two rules. ParityChunks+2 keeps one domain spare, so a single add
// or remove after a domain's loss still has somewhere to relocate a role.
// DataChunks+ParityChunks gives each chunk of a stripe a domain of its own. The
// first binds up to 2+2, the second for 4+1 (five) and 4+2 (six).
func (s Scheme) MinimumFailureDomains() int {
	return max(s.ParityChunks+2, s.DataChunks+s.ParityChunks)
}

// failureDomainRule names the rule that sets the scheme's minimum, for a
// refusal to cite.
func failureDomainRule(s Scheme) string {
	if s.DataChunks+s.ParityChunks > s.ParityChunks+2 {
		return "one per chunk of the stripe"
	}
	return "the stripe's parity plus two"
}

// FailureDomainViolation answers a layout, the hosts in each failure domain,
// against what a fresh activation accepts: at least MinimumFailureDomains
// distinct domains and the same number of hosts in every domain. It returns the
// reason the layout is refused, or an empty string for one that is accepted.
func FailureDomainViolation(scheme Scheme, hostsPerDomain map[string]int) string {
	if refused := DomainCountViolation(scheme, len(hostsPerDomain)); refused != "" {
		return fmt.Sprintf("%s, and the workers are in %d (%s)",
			refused, len(hostsPerDomain), describeLayout(hostsPerDomain))
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

// DomainCountViolation answers the number of distinct failure domains alone,
// for a caller that counts them from reported nodes rather than from a planned
// layout. It returns the reason the count is refused, or an empty string.
func DomainCountViolation(scheme Scheme, domains int) string {
	minimum := scheme.MinimumFailureDomains()
	if domains >= minimum {
		return ""
	}
	return fmt.Sprintf("failure-domain mode with %s needs at least %d distinct failure domains (%s)",
		scheme, minimum, failureDomainRule(scheme))
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
