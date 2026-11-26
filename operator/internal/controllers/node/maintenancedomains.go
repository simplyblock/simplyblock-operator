// What a maintenance window counts as one unit of unavailability.
//
// The concurrency gate of a maintenance window counts workers, because a worker
// is what a drain takes away. On a cluster in failure-domain mode that is too
// small a unit. The control plane places the chunks of a stripe in different
// failure domains, and with at least as many domains as a stripe has chunks a
// domain holds one chunk of any stripe at most. Every worker of one domain
// being down then costs a stripe exactly what one worker being down costs it,
// so the domain is the unit, and its workers go into maintenance together the
// way the sockets of one host do.
//
// With fewer domains than chunks a domain holds several chunks of a stripe,
// two of its workers down are two chunks lost, and the unit stays the worker.
// The control plane applies the same rule to a shutdown, and refuses the one
// that breaks it; this file is why the operator does not ask for it.
//
// design-storagenode.md §10.

package node

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/utils"
)

// maintenanceLayout is the cluster's nodes as the maintenance gates read them.
type maintenanceLayout struct {
	// nodes are the cluster's own nodes that have not been removed. A remote
	// node is the peer site's, and its maintenance is the peer site's too.
	nodes []simplyblockv1alpha2.StorageNode

	// byDomain says a whole failure domain is one unit of unavailability.
	byDomain bool
}

// maintenanceLayout reads the cluster's nodes and decides what the unit is.
func (r *StorageNodeOpsReconciler) maintenanceLayout(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) (maintenanceLayout, error) {
	var list simplyblockv1alpha2.StorageNodeList
	if err := r.List(ctx, &list, client.InNamespace(cluster.Namespace)); err != nil {
		return maintenanceLayout{}, fmt.Errorf("list the nodes of cluster %s: %w", cluster.Name, err)
	}

	var layout maintenanceLayout
	for i := range list.Items {
		node := &list.Items[i]
		if node.Spec.ClusterRef != cluster.Name || node.IsRemote() ||
			node.Status.Status == utils.NodeStatusRemoved {
			continue
		}
		layout.nodes = append(layout.nodes, *node)
	}
	layout.byDomain = ptr.BoolFromOrFalse(cluster.Spec.EnableFailureDomains) &&
		oneChunkPerDomain(cluster.Status.ErasureCodingScheme, layout.nodes)
	return layout, nil
}

// oneChunkPerDomain reports whether the nodes' failure domains are enough for
// a domain to hold one chunk of a stripe at most.
//
// A layout that cannot be read, one with no parity, and a node that declares
// no domain all answer no. Each is a cluster whose placement this cannot vouch
// for, and counting workers is what is safe on one of those.
func oneChunkPerDomain(scheme string, nodes []simplyblockv1alpha2.StorageNode) bool {
	chunks, err := utils.RequiredNodesFromErasureCodingScheme(scheme)
	if err != nil {
		return false
	}
	parity, err := utils.ParityChunksFromErasureCodingScheme(scheme)
	if err != nil || parity < 1 {
		return false
	}

	domains := map[string]struct{}{}
	for i := range nodes {
		domain := nodes[i].Spec.Config.FailureDomain
		if domain == "" {
			return false
		}
		domains[domain] = struct{}{}
	}
	return len(domains) >= chunks
}

// unitOf names what the node's unavailability is counted as: its failure
// domain where a domain is the unit, and its worker everywhere else.
func (l maintenanceLayout) unitOf(node *simplyblockv1alpha2.StorageNode) string {
	if l.byDomain {
		return node.Spec.Config.FailureDomain
	}
	return node.Spec.WorkerNode
}

// downOnlyIn reports whether every node of the cluster that is not online
// belongs to the given node's failure domain, on a cluster where a domain is
// the unit.
//
// It is what tells a cluster degraded by this window's own domain from one
// degraded by another. A node whose status is unknown is not online, and it
// counts against the answer wherever it is: a node this cannot vouch for is
// not one to take a second domain down beside.
func (l maintenanceLayout) downOnlyIn(node *simplyblockv1alpha2.StorageNode) bool {
	if !l.byDomain {
		return false
	}
	for i := range l.nodes {
		other := &l.nodes[i]
		if other.Name == node.Name || other.Status.Status == nodeStatusOnline {
			continue
		}
		if other.Spec.Config.FailureDomain != node.Spec.Config.FailureDomain {
			return false
		}
	}
	return true
}

// inTransition names a node of the cluster, other than the given one, that the
// control plane is shutting down or restarting.
//
// The control plane takes one node through a shutdown at a time and refuses a
// second while the first is in flight, whatever domain either is in.
func (l maintenanceLayout) inTransition(node *simplyblockv1alpha2.StorageNode) (string, bool) {
	for i := range l.nodes {
		other := &l.nodes[i]
		if other.Name == node.Name {
			continue
		}
		if other.Status.Status == nodeStatusInShutdown || other.Status.Status == nodeStatusInRestart {
			return other.Name, true
		}
	}
	return "", false
}
