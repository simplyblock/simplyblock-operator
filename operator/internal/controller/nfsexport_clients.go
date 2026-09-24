// Which hosts may mount an export, and the address they mount it at.
//
// The set goes verbatim into an exports(5) entry, so it is the only thing
// between a shared filesystem and every host that can reach the metadata
// server. Nothing widens on a missing input: an empty set is a visible wait,
// while a wildcard fallback is a hole nobody notices.
//
// Derived every pass rather than stored, because it changes as nodes come and
// go and it belongs in status. Specified by design-pnfs-rwx.md §7.1.

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// clusterNodeAddresses is every node that could run a pod using this volume.
//
// Wider than the design's "nodes currently running a consuming pod" and
// narrower than everything. Narrowing to actual placement means rewriting the
// entry as pods move, and an entry rewritten under a live mount is a client
// that loses its export mid-write.
func (r *NFSExportReconciler) clusterNodeAddresses(ctx context.Context) ([]string, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing cluster nodes for the export's client set: %w", err)
	}
	clients := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		if addr := internalAddress(&nodes.Items[i]); addr != "" {
			clients = append(clients, addr)
		}
	}
	return clients, nil
}

// nodeAddress is the bound MDS host's own address: what the export's Service
// EndpointSlice points at, not what a client mounts (see reconcileExportService
// and ServiceAddress). "" when the node is gone or publishes none.
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
	return internalAddress(&node), nil
}

// internalAddress is the one address type a client can reach the node at.
func internalAddress(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address
		}
	}
	return ""
}
