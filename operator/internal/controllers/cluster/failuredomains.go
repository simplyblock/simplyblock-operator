// The failure-domain admission rules, mirrored from the control plane so the
// operator refuses what the backend would refuse and refuses it earlier.
//
// They live here rather than beside either caller because they are properties
// of a cluster's layout, computed across every node beneath it, and three
// reconcilers ask them: this package's activation gate, the storage-node
// operation's removal gate, and the node set's activation trigger. Each rule
// mirrors a named function on the Python side, and the pairing is stated on
// each so the two cannot quietly drift.
//
// They are exported for that reason, and only for that reason: nothing outside
// the operator reads them.

package cluster

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// FailureDomainHosts aggregates each host's failure domain, by management IP,
// across every StorageNodeSet belonging to clusterName in namespace.
//
// A host with no domain reported yet is skipped rather than counted as domain
// zero, and a node already removed is skipped too. Both mirror
// simplyblock_core's failure_domain_host_map, which excludes
// StorageNode.STATUS_REMOVED for the same reason: a removed node's stale domain
// assignment must not inflate that domain's apparent host count for either the
// activation gate below or the removal gate beside it.
func FailureDomainHosts(
	ctx context.Context, c client.Client, namespace, clusterName string,
) (map[string]int32, error) {
	var sets simplyblockv1alpha1.StorageNodeSetList
	if err := c.List(ctx, &sets, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	hostDomains := map[string]int32{}
	for _, set := range sets.Items {
		if set.Spec.ClusterName != clusterName {
			continue
		}
		for _, node := range set.Status.Nodes {
			if node.FailureDomain == nil || node.MgmtIp == "" ||
				node.Status == utils.NodeStatusRemoved {
				continue
			}
			hostDomains[node.MgmtIp] = *node.FailureDomain
		}
	}
	return hostDomains, nil
}

// HostHasSurvivingSibling reports whether any storage node other than
// excludeUUID is still reported at mgmtIP within clusterName.
//
// A multi-node host runs more than one storage node, all sharing one
// management IP in FailureDomainHosts' per-host map, so a caller must not treat
// the whole host as gone when only one of its nodes is being removed.
//
// It fails closed on a List error, reporting no confirmed sibling, so the
// caller's default stays "assume the host disappears" rather than silently
// skipping a real single-node removal.
func HostHasSurvivingSibling(
	ctx context.Context, c client.Client, namespace, clusterName, mgmtIP, excludeUUID string,
) bool {
	var sets simplyblockv1alpha1.StorageNodeSetList
	if err := c.List(ctx, &sets, client.InNamespace(namespace)); err != nil {
		return false
	}
	for _, set := range sets.Items {
		if set.Spec.ClusterName != clusterName {
			continue
		}
		for _, node := range set.Status.Nodes {
			if node.MgmtIp == mgmtIP && node.UUID != excludeUUID &&
				node.Status != utils.NodeStatusRemoved {
				return true
			}
		}
	}
	return false
}

// RemovalBalanceViolation validates a set of post-removal per-domain host
// counts against the balance rule and per-domain floor the backend enforces in
// simplyblock_core's fd_balance_violation, which check_fd_admission_for_remove
// calls. It is mirrored here so the drain reconciler can refuse to suspend a
// node whose removal is already known to violate the rule, rather than
// discovering that only after the node is suspended and the DELETE made much
// later in the drain is rejected.
//
// counts must already reflect the removal, with the affected domain's count
// decremented by the caller, and must still carry an entry — even a zero one —
// for every domain that had at least one host before the removal. A domain
// silently absent from the map is indistinguishable from one that never
// existed, and both the balance and the floor check would miss a domain
// dropping out entirely.
//
// It returns a human-readable reason on a violation and the empty string when
// the counts are acceptable.
func RemovalBalanceViolation(counts map[int32]int) string {
	if len(counts) == 0 {
		return ""
	}
	const maxDelta = 1
	const minHostsPerFD = 2
	lo, hi := -1, -1
	for _, count := range counts {
		if lo == -1 || count < lo {
			lo = count
		}
		if hi == -1 || count > hi {
			hi = count
		}
	}
	if hi-lo > maxDelta {
		return fmt.Sprintf(
			"failure domains would be unbalanced by %d hosts (allowed: %d); populations: %v",
			hi-lo, maxDelta, counts)
	}
	for domain, count := range counts {
		if count < minHostsPerFD {
			return fmt.Sprintf(
				"failure domain %d would drop to %d host(s); at least %d are required per domain",
				domain, count, minHostsPerFD)
		}
	}
	return ""
}

// ActivationDomainCountViolation validates the number of distinct failure
// domains and the per-domain host balance for a fresh activation, mirroring
// simplyblock_core's fd_activation_domain_count_violation so the two stay in
// lockstep.
//
// A two-domain layout can never absorb a second independent failure once one
// domain is fully down, so a fresh activation requires npcs+2 distinct domains
// — three for npcs=1, four for npcs=2 — with an equal host count in each. Below
// npcs+1 domains even the initial static role-placement rotation is
// structurally wrong; at exactly npcs+1 it is correct but has no spare capacity
// for a later add or remove.
//
// It returns a human-readable reason when the cluster is not ready and the
// empty string when it is.
func ActivationDomainCountViolation(npcs int, hostDomains map[string]int32) string {
	if len(hostDomains) == 0 {
		return "no storage nodes with a failure-domain assignment reported yet"
	}
	counts := map[int32]int{}
	for _, domain := range hostDomains {
		counts[domain]++
	}
	minDomains := npcs + 2
	if len(counts) < minDomains {
		return fmt.Sprintf(
			"failure domains are enabled with npcs=%d, which requires at least "+
				"%d distinct failure domains (2 domains is not supported at any "+
				"npcs level); currently have %d",
			npcs, minDomains, len(counts))
	}
	first := -1
	for _, count := range counts {
		if first == -1 {
			first = count
			continue
		}
		if count != first {
			return fmt.Sprintf(
				"failure domains must hold an equal number of hosts at "+
					"activation; current split: %v", counts)
		}
	}
	return ""
}
