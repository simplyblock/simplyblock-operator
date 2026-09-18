// The activation gate for a cluster whose storage nodes are too few for its
// erasure-coding scheme.
//
// Unlike the failure-domain rules next door, this one is not mirrored from the
// control plane, because the control plane does not have it. Its own activation
// gate counts devices — ndcs+npcs+1 of them — and never nodes, so a fleet of
// four configured 4+2 passes it: six devices are easy on four nodes, and the
// stripe still has nowhere to place its chunks and nothing to rebuild onto. The
// rule enforced here is the one the product documentation states, which nothing
// in the system enforced until this file.
//
// It gates an activation rather than a cluster create, because that is where the
// layout stops being a proposal: a StorageCluster is created before its nodes
// exist, so a node count at create time is a number about nothing. The
// deployment config's validation and its approval webhook answer the same
// question earlier, against the document; this answers it for the cluster and
// the nodes themselves, however they were written.

package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/erasurecoding"
)

// ActivationNodeCountViolation reports why a cluster with this many storage
// nodes must not be activated with this stripe, and the empty string when it
// may be.
//
// An unstated stripe is 1+1, which is what the control plane defaults to, so a
// cluster that says nothing about erasure coding is held to the three nodes 1+1
// needs rather than to nothing.
func ActivationNodeCountViolation(stripe *simplyblockv1alpha2.StripeSpec, nodes int) string {
	scheme := erasurecoding.SchemeOf(stripe)
	minimum := scheme.MinimumNodes()
	if nodes >= minimum {
		return ""
	}

	if scheme.ParityChunks == 0 {
		return fmt.Sprintf(
			"the cluster's stripe is %s, which needs at least %d storage node, and the "+
				"cluster has %d", scheme, minimum, nodes)
	}
	return fmt.Sprintf(
		"the cluster's stripe is %s, which needs at least %d storage nodes (%d to place "+
			"a stripe across and %d to rebuild onto after a failure), and the cluster "+
			"has %d", scheme, minimum, scheme.DataChunks+scheme.ParityChunks,
		scheme.ParityChunks, nodes)
}

// stripeNodesReady gates an activation on the cluster having the storage nodes
// its scheme requires, and reports the hold rather than failing.
//
// It holds rather than fails for the reason the failure-domain gate beside it
// does: a cluster whose nodes are still being created is a cluster that will
// meet the rule shortly, and an operation that failed on a count taken too early
// is one somebody has to notice and re-issue. What it counts is the StorageNode
// objects of the cluster rather than what the control plane reports, because the
// objects are what the deployment states it will have; whether each of them is
// online is the expansion's own gate, one step earlier.
func (r *StorageClusterOpsReconciler) stripeNodesReady(
	ctx context.Context, ops *simplyblockv1alpha2.StorageClusterOps,
) (bool, error) {
	var cluster simplyblockv1alpha2.StorageCluster
	key := client.ObjectKey{Name: ops.Spec.ClusterRef, Namespace: ops.Namespace}
	if err := r.Get(ctx, key, &cluster); err != nil {
		return false, err
	}

	nodes, err := storageNodeCount(ctx, r.Client, cluster.Namespace, cluster.Name)
	if err != nil {
		return false, err
	}

	reason := ActivationNodeCountViolation(cluster.Spec.Stripe, nodes)
	if reason == "" {
		return true, nil
	}

	r.Recorder.Eventf(ops, nil, corev1.EventTypeWarning,
		StripeNodesNotReady, StripeNodesNotReady,
		"The activation is waiting on the nodes the erasure coding requires: %s", reason)
	return false, nil
}

// storageNodeCount is how many storage nodes belong to one cluster.
func storageNodeCount(
	ctx context.Context, c client.Client, namespace, clusterName string,
) (int, error) {
	var nodes simplyblockv1alpha2.StorageNodeList
	if err := c.List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("read the nodes of cluster %s: %w", clusterName, err)
	}
	count := 0
	for i := range nodes.Items {
		if nodes.Items[i].Spec.ClusterRef == clusterName {
			count++
		}
	}
	return count, nil
}
