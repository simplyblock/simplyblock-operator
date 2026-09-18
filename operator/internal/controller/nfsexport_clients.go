// Resolving spec.clientPolicy into the set of hosts allowed to mount an export.
//
// The result is written verbatim into an exports(5) entry, so it is the only
// thing between a shared filesystem and every host that can reach the metadata
// server. That is why nothing here widens on a missing input: a policy that
// resolves to nothing publishes to nobody, which is a visible wait, while a
// policy that fell back to a wildcard would be a hole nobody would notice.
//
// It is derived on every pass rather than stored, because it changes as nodes
// join and leave and it belongs in status: an effective set that bumped
// generation would make a scheduling event look like a spec edit.
//
// Specified by operator/docs/designs/design-pnfs-rwx.md §7.1.

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// allowedClients resolves the policy against the cluster as it is now.
func (r *NFSExportReconciler) allowedClients(
	ctx context.Context,
	export *simplyblockv1alpha2.NFSExport,
) ([]string, error) {
	switch clientPolicyMode(export) {
	case simplyblockv1alpha2.NFSExportClientPolicyModeOpen:
		// The only wildcard, and only ever reached by asking for it.
		return []string{"*"}, nil

	case simplyblockv1alpha2.NFSExportClientPolicyModeSubnet:
		// Exactly what was written. A Subnet policy naming no subnet resolves
		// to nothing rather than falling back to the cluster's own nodes: a
		// policy that says which CIDRs may mount has said that none may.
		if policy := export.Spec.ClientPolicy; policy != nil {
			return append([]string{}, policy.Subnets...), nil
		}
		return nil, nil

	default:
		return r.clusterNodeAddresses(ctx)
	}
}

// clientPolicyMode applies the CRD's default, so code reading the policy does
// not have to care whether admission had a chance to default it.
func clientPolicyMode(
	export *simplyblockv1alpha2.NFSExport,
) simplyblockv1alpha2.NFSExportClientPolicyMode {
	if p := export.Spec.ClientPolicy; p != nil && p.Mode != "" {
		return p.Mode
	}
	return simplyblockv1alpha2.NFSExportClientPolicyModeNodeScoped
}

// clusterNodeAddresses is the NodeScoped set: the internal address of every node
// that could run a pod using this volume.
//
// It is wider than the design's "nodes currently running a consuming pod" and
// narrower than everything. Narrowing it to actual placement means rewriting the
// export entry as pods move, and an entry rewritten under a live mount is a
// client that loses its export mid-write, so the refinement needs a drain of its
// own and is deliberately not attempted here.
func (r *NFSExportReconciler) clusterNodeAddresses(ctx context.Context) ([]string, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing cluster nodes for the client policy: %w", err)
	}
	clients := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		for _, addr := range nodes.Items[i].Status.Addresses {
			if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
				clients = append(clients, addr.Address)
				break
			}
		}
	}
	return clients, nil
}

// nodeAddress is the internal address of one Kubernetes node, or an empty
// string when the node is gone or publishes none.
//
// This is the address a client mounts the export at. Design §13.3 puts a
// Service in front of the metadata server so the address survives a move, and
// that belongs with failover: until an export can move, a Service would add an
// indirection whose only purpose is to absorb a change that cannot happen yet.
// What matters now is that the field is filled at all, because an empty one
// fails at the client with a message about the mount rather than about the host.
func (r *NFSExportReconciler) nodeAddress(ctx context.Context, nodeName string) (string, error) {
	if nodeName == "" {
		return "", nil
	}
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading node %s for the export address: %w", nodeName, err)
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address, nil
		}
	}
	return "", nil
}
